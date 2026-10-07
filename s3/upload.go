package s3

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	sdk "github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"golang.org/x/sync/errgroup"
)

type api interface {
	PutObject(context.Context, *sdk.PutObjectInput, ...func(*sdk.Options)) (*sdk.PutObjectOutput, error)
	CreateMultipartUpload(context.Context, *sdk.CreateMultipartUploadInput, ...func(*sdk.Options)) (*sdk.CreateMultipartUploadOutput, error)
	UploadPart(context.Context, *sdk.UploadPartInput, ...func(*sdk.Options)) (*sdk.UploadPartOutput, error)
	ListParts(context.Context, *sdk.ListPartsInput, ...func(*sdk.Options)) (*sdk.ListPartsOutput, error)
	CompleteMultipartUpload(context.Context, *sdk.CompleteMultipartUploadInput, ...func(*sdk.Options)) (*sdk.CompleteMultipartUploadOutput, error)
	AbortMultipartUpload(context.Context, *sdk.AbortMultipartUploadInput, ...func(*sdk.Options)) (*sdk.AbortMultipartUploadOutput, error)
	HeadObject(context.Context, *sdk.HeadObjectInput, ...func(*sdk.Options)) (*sdk.HeadObjectOutput, error)
}

type Uploader struct {
	client api
	config Config
}
type Progress struct {
	BytesWritten  int64
	ContentLength int64
	Percentage    float64
}
type Result struct {
	Backend   string
	Bucket    string
	Key       string
	ETag      string
	Hash      string
	Duplicate bool
}
type checkpoint struct {
	UploadID    string `json:"upload_id"`
	Fingerprint string `json:"fingerprint"`
	Token       string `json:"token"`
	Size        int64  `json:"size"`
	PartSize    int64  `json:"part_size"`
}

func New(c Config) (*Uploader, error) {
	if c.S3.Concurrency == 0 {
		c.S3.Concurrency = 4
	}
	if c.S3.PartSize == 0 {
		c.S3.PartSize = 8 << 20
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	client := sdk.New(sdk.Options{
		Region: c.S3.Region, BaseEndpoint: aws.String(c.S3.Endpoint), UsePathStyle: true,
		Credentials:                credentials.NewStaticCredentialsProvider(c.S3.AccessKeyID, c.S3.SecretAccessKey, ""),
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
		ResponseChecksumValidation: aws.ResponseChecksumValidationWhenRequired,
	})
	return &Uploader{client: client, config: c}, nil
}

// UploadFile resumes previously acknowledged parts. Cancellation retains remote
// parts and local state; call Abort to discard them. Progress counts whole parts.
func (u *Uploader) UploadFile(ctx context.Context, path, key string, progress func(Progress)) (Result, error) {
	return u.uploadFile(ctx, path, key, progress, "")
}

func (u *Uploader) uploadFile(ctx context.Context, path, key string, progress func(Progress), knownHash string) (Result, error) {
	result := Result{Backend: "s3", Bucket: u.config.S3.Bucket, Key: key}
	if key == "" {
		return result, fmt.Errorf("object key is required")
	}
	statePath, unlock, err := u.lock(key)
	if err != nil {
		return result, err
	}
	defer unlock()
	file, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() {
		return result, fmt.Errorf("source must be a regular file")
	}
	fingerprint := knownHash
	if fingerprint == "" {
		fingerprint, err = hashFile(ctx, file)
		if err != nil {
			return result, err
		}
	}
	result.Hash = fingerprint
	state, err := readState(statePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	if err == nil {
		if state.Fingerprint != fingerprint || state.Size != info.Size() {
			return result, fmt.Errorf("source changed since upload started; abort the previous upload first")
		}
		if state.PartSize < 5<<20 || state.PartSize > 5<<30 || state.UploadID == "" || state.Token == "" {
			return result, fmt.Errorf("invalid upload checkpoint")
		}
	} else {
		if info.Size() == 0 {
			out, err := u.client.PutObject(ctx, &sdk.PutObjectInput{
				Bucket: aws.String(result.Bucket), Key: aws.String(key),
				Body: io.NewSectionReader(file, 0, 0), ContentLength: aws.Int64(0),
				Metadata: map[string]string{"sha256": fingerprint},
			})
			if err != nil {
				return result, fmt.Errorf("upload empty file: %w", err)
			}
			result.ETag = aws.ToString(out.ETag)
			if progress != nil {
				progress(Progress{0, 0, 100})
			}
			return result, nil
		}
		partSize := u.config.S3.PartSize
		if info.Size() > partSize*10000 {
			partSize = (info.Size() + 9999) / 10000
		}
		if partSize > 5<<30 {
			return result, fmt.Errorf("file exceeds multipart limits")
		}
		token := make([]byte, 16)
		if _, err := rand.Read(token); err != nil {
			return result, err
		}
		state = checkpoint{Fingerprint: fingerprint, Size: info.Size(), PartSize: partSize, Token: hex.EncodeToString(token)}
		out, err := u.client.CreateMultipartUpload(ctx, &sdk.CreateMultipartUploadInput{
			Bucket: aws.String(result.Bucket), Key: aws.String(key), Metadata: map[string]string{"upload-token": state.Token, "sha256": fingerprint},
		})
		if err != nil {
			return result, fmt.Errorf("initiate upload: %w", err)
		}
		state.UploadID = aws.ToString(out.UploadId)
		if state.UploadID == "" {
			return result, fmt.Errorf("server returned an empty upload ID")
		}
		if err := saveState(statePath, state); err != nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			_, abortErr := u.client.AbortMultipartUpload(cleanupCtx, &sdk.AbortMultipartUploadInput{Bucket: aws.String(result.Bucket), Key: aws.String(key), UploadId: aws.String(state.UploadID)})
			return result, errors.Join(err, abortErr)
		}
	}
	parts := map[int32]types.Part{}
	paginator := sdk.NewListPartsPaginator(u.client, &sdk.ListPartsInput{Bucket: aws.String(result.Bucket), Key: aws.String(key), UploadId: aws.String(state.UploadID)})
	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			// Completion may have succeeded before its response reached the caller.
			if apiCode(err, "NoSuchUpload") {
				head, headErr := u.client.HeadObject(ctx, &sdk.HeadObjectInput{Bucket: aws.String(result.Bucket), Key: aws.String(key)})
				if headErr == nil && head.Metadata["upload-token"] == state.Token && aws.ToInt64(head.ContentLength) == state.Size {
					result.ETag = aws.ToString(head.ETag)
					return result, u.finish(statePath, state.Size, progress)
				}
			}
			return result, fmt.Errorf("list upload parts (checkpoint retained): %w", err)
		}
		for _, part := range page.Parts {
			parts[aws.ToInt32(part.PartNumber)] = part
		}
	}
	count := (state.Size + state.PartSize - 1) / state.PartSize
	if count > 10000 {
		return result, fmt.Errorf("checkpoint exceeds multipart part limit")
	}
	if count == 0 {
		count = 1
	}
	completed := make([]types.CompletedPart, count)
	var written int64
	report := func() {
		if progress != nil {
			percent := float64(0)
			if state.Size > 0 {
				percent = 100 * float64(written) / float64(state.Size)
			}
			progress(Progress{written, state.Size, percent})
		}
	}
	// Count remotely acknowledged parts before sending anything else.
	for n := int64(1); n <= count; n++ {
		size := min(state.PartSize, state.Size-(n-1)*state.PartSize)
		part := parts[int32(n)]
		if aws.ToInt64(part.Size) == size && aws.ToString(part.ETag) != "" {
			written += size
		}
	}
	report()
	group, uploadCtx := errgroup.WithContext(ctx)
	group.SetLimit(u.config.S3.Concurrency)
	var progressMu sync.Mutex
	for n := int64(1); n <= count; n++ {
		offset := (n - 1) * state.PartSize
		size := min(state.PartSize, state.Size-offset)
		part := parts[int32(n)]
		etag := aws.ToString(part.ETag)
		if etag != "" && aws.ToInt64(part.Size) == size {
			completed[n-1] = types.CompletedPart{PartNumber: aws.Int32(int32(n)), ETag: aws.String(etag)}
			continue
		}
		group.Go(func() error {
			if err := uploadCtx.Err(); err != nil {
				return err
			}
			out, err := u.client.UploadPart(uploadCtx, &sdk.UploadPartInput{
				Bucket: aws.String(result.Bucket), Key: aws.String(key), UploadId: aws.String(state.UploadID), PartNumber: aws.Int32(int32(n)),
				Body: io.NewSectionReader(file, offset, size), ContentLength: aws.Int64(size),
			})
			if err != nil {
				return fmt.Errorf("upload part %d (checkpoint retained): %w", n, err)
			}
			etag = aws.ToString(out.ETag)
			if etag == "" {
				return fmt.Errorf("part %d returned empty ETag", n)
			}
			completed[n-1] = types.CompletedPart{PartNumber: aws.Int32(int32(n)), ETag: aws.String(etag)}
			progressMu.Lock()
			written += size
			report()
			progressMu.Unlock()
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return result, err
	}
	// Detect modifications during the upload before publishing a mixed object.
	finalHash, err := hashFile(ctx, file)
	if err != nil {
		return result, err
	}
	if finalHash != state.Fingerprint {
		return result, fmt.Errorf("source changed during upload; abort before retrying")
	}
	out, err := u.client.CompleteMultipartUpload(ctx, &sdk.CompleteMultipartUploadInput{
		Bucket: aws.String(result.Bucket), Key: aws.String(key), UploadId: aws.String(state.UploadID), MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	if err != nil {
		return result, fmt.Errorf("complete upload (checkpoint retained): %w", err)
	}
	result.ETag = aws.ToString(out.ETag)
	return result, u.finish(statePath, state.Size, progress)
}

// Abort removes the saved multipart upload. No source file is needed.
func (u *Uploader) Abort(ctx context.Context, key string) error {
	if key == "" {
		return fmt.Errorf("object key is required")
	}
	path, unlock, err := u.lock(key)
	if err != nil {
		return err
	}
	defer unlock()
	state, err := readState(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = u.client.AbortMultipartUpload(ctx, &sdk.AbortMultipartUploadInput{Bucket: aws.String(u.config.S3.Bucket), Key: aws.String(key), UploadId: aws.String(state.UploadID)})
	if err != nil && !apiCode(err, "NoSuchUpload") {
		return err
	}
	return os.Remove(path)
}

func (u *Uploader) lock(key string) (string, func(), error) {
	dir := filepath.Join(u.config.StoragePath, ".s3-uploads")
	return u.lockInDir(dir, key)
}

func (u *Uploader) lockInDir(dir, key string) (string, func(), error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", nil, err
	}
	id := sha256.Sum256([]byte(u.config.S3.Endpoint + "\x00" + u.config.S3.Region + "\x00" + u.config.S3.Bucket + "\x00" + key))
	path := filepath.Join(dir, hex.EncodeToString(id[:])+".json")
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return "", nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return "", nil, fmt.Errorf("another upload for this object is active: %w", err)
	}
	return path, func() { syscall.Flock(int(file.Fd()), syscall.LOCK_UN); file.Close() }, nil
}

func readState(path string) (checkpoint, error) {
	var state checkpoint
	data, err := os.ReadFile(path)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("decode checkpoint: %w", err)
	}
	return state, nil
}
func saveState(path string, state checkpoint) error {
	return saveJSON(path, state)
}
func saveJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".checkpoint-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
func hashFile(ctx context.Context, file *os.File) (string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	h := sha256.New()
	buffer := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := file.Read(buffer)
		h.Write(buffer[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func apiCode(err error, code string) bool {
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == code
}
func (u *Uploader) finish(path string, size int64, progress func(Progress)) error {
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("upload completed but checkpoint cleanup failed: %w", err)
	}
	if progress != nil {
		progress(Progress{size, size, 100})
	}
	return nil
}
