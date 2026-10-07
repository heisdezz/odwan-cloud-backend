package s3

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	sdk "github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

var ErrInvalidHash = errors.New("hash must be a 64-character SHA-256 hex string")
var ErrHashMismatch = errors.New("provided hash does not match the file SHA-256")

type hashEntry struct {
	Key string `json:"key"`
}

// UploadFileUnique verifies an optional expected SHA-256 and avoids sending
// content already recorded for this bucket. Hash records persist across restarts.
func (u *Uploader) UploadFileUnique(ctx context.Context, source, key, expectedHash string, progress func(Progress)) (Result, error) {
	expectedHash = strings.ToLower(strings.TrimSpace(expectedHash))
	if expectedHash != "" {
		decoded, err := hex.DecodeString(expectedHash)
		if err != nil || len(decoded) != 32 {
			return Result{}, ErrInvalidHash
		}
	}
	file, err := os.Open(source)
	if err != nil {
		return Result{}, err
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return Result{}, err
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return Result{}, fmt.Errorf("source must be a regular file")
	}
	fingerprint, err := hashFile(ctx, file)
	file.Close()
	if err != nil {
		return Result{}, err
	}
	if expectedHash != "" && expectedHash != fingerprint {
		return Result{}, ErrHashMismatch
	}
	if key == "" {
		return Result{}, fmt.Errorf("object key is required")
	}
	indexPath, unlock, err := u.lockInDir(filepath.Join(u.config.StoragePath, ".s3-hashes"), fingerprint)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	data, err := os.ReadFile(indexPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, err
	}
	var existing hashEntry
	if err == nil {
		if err := json.Unmarshal(data, &existing); err != nil {
			return Result{}, fmt.Errorf("decode hash index: %w", err)
		}
	}
	// Also recognize older uploads at the requested key, even without an index.
	candidates := []string{key}
	if existing.Key != "" && existing.Key != key {
		candidates = append([]string{existing.Key}, candidates...)
	}
	for _, candidate := range candidates {
		head, err := u.client.HeadObject(ctx, &sdk.HeadObjectInput{Bucket: aws.String(u.config.S3.Bucket), Key: aws.String(candidate)})
		if err != nil {
			if missingObject(err) {
				continue
			}
			return Result{}, fmt.Errorf("check duplicate object: %w", err)
		}
		if head.Metadata["sha256"] == fingerprint && aws.ToInt64(head.ContentLength) == info.Size() {
			if err := u.clearCompletedState(candidate, fingerprint, head.Metadata["upload-token"]); err != nil {
				return Result{}, err
			}
			if err := saveJSON(indexPath, hashEntry{Key: candidate}); err != nil {
				return Result{}, err
			}
			if progress != nil {
				progress(Progress{info.Size(), info.Size(), 100})
			}
			return Result{Backend: "s3", Bucket: u.config.S3.Bucket, Key: candidate, ETag: aws.ToString(head.ETag), Hash: fingerprint, Duplicate: true}, nil
		}
	}
	result, err := u.uploadFile(ctx, source, key, progress, fingerprint)
	if err != nil {
		return result, err
	}
	if err := saveJSON(indexPath, hashEntry{Key: result.Key}); err != nil {
		return result, fmt.Errorf("upload completed but hash index save failed: %w", err)
	}
	return result, nil
}

// Clear a checkpoint left behind by a lost completion response, without touching
// a newer multipart upload for the same key.
func (u *Uploader) clearCompletedState(key, hash, token string) error {
	if token == "" {
		return nil
	}
	statePath, unlock, err := u.lock(key)
	if err != nil {
		return nil
	} // An active uploader owns its own checkpoint.
	defer unlock()
	state, err := readState(statePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if state.Fingerprint == hash && state.Token == token {
		return os.Remove(statePath)
	}
	return nil
}

func missingObject(err error) bool {
	var responseErr *smithyhttp.ResponseError
	return apiCode(err, "NotFound") || apiCode(err, "NoSuchKey") || apiCode(err, "404") || (errors.As(err, &responseErr) && responseErr.HTTPStatusCode() == 404)
}
