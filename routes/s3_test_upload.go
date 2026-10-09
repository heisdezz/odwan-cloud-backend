package routes

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"

	"go-test/s3"
	"go-test/telegramstorage"

	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

const testUploadBodyLimit int64 = 256 << 20

type fileUploader interface {
	UploadFileUnique(context.Context, string, string, string, func(s3.Progress)) (s3.Result, error)
}

// RegisterStorageRoutes registers streaming and, when test_token is set, test uploads.
func RegisterStorageRoutes(se *core.ServeEvent, config s3.Config) error {

	registry := &storageRegistry{config: config, clients: map[string]fileUploader{}}
	resolver := func(backend string) (objectStreamer, error) {
		uploader, err := registry.get(backend)
		if err != nil {
			return nil, err
		}
		streamer, ok := uploader.(objectStreamer)
		if !ok {
			return nil, fmt.Errorf("backend cannot stream")
		}
		return streamer, nil
	}
	handler := mediaStreamHandler(resolver, config.TestToken)
	thumbnail := newThumbnailService(resolver).handler()
	se.Router.GET("/api/media/{id}/thumb", thumbnail)
	se.Router.HEAD("/api/media/{id}/thumb", thumbnail)
	se.Router.GET("/api/media/{id}/stream", handler)
	se.Router.HEAD("/api/media/{id}/stream", handler)
	if config.TestToken != "" {
		registerTestMediaRoutes(se, config.TestToken, handler, thumbnail)
		uploader, err := registry.get(config.StorageBackend)
		if err != nil {
			return fmt.Errorf("test uploader: %w", err)
		}
		stagingDir := filepath.Join(config.StoragePath, ".s3-test-inputs")
		if err := os.MkdirAll(stagingDir, 0700); err != nil {
			registry.close()
			return err
		}
		upload := testUploadHandler(uploader, stagingDir, config.TestToken)
		se.Router.POST("/api/test/s3/upload", func(e *core.RequestEvent) error {
			registry.lifecycle.Lock()
			defer registry.lifecycle.Unlock()
			return upload(e)
		}).Bind(apis.BodyLimit(testUploadBodyLimit))
	}
	cancelCleanup, cleanupDone := registerMediaCleanup(se, registry)
	se.App.OnTerminate().BindFunc(func(e *core.TerminateEvent) error {
		cancelCleanup()
		<-cleanupDone
		return errors.Join(e.Next(), registry.close())
	})

	return nil
}

func testUploadHandler(uploader fileUploader, stagingDir, token string) func(*core.RequestEvent) error {
	return func(re *core.RequestEvent) error {
		if subtle.ConstantTimeCompare([]byte(re.Request.Header.Get("X-S3-Test-Token")), []byte(token)) != 1 {
			return re.JSON(http.StatusUnauthorized, map[string]string{"error": "invalid test token"})
		}
		re.Request.Body = http.MaxBytesReader(re.Response, re.Request.Body, testUploadBodyLimit)
		err := re.Request.ParseMultipartForm(8 << 20)
		if re.Request.MultipartForm != nil {
			defer re.Request.MultipartForm.RemoveAll()
		}
		if err != nil {
			var limitErr *http.MaxBytesError
			if errors.As(err, &limitErr) || errors.Is(err, apis.ErrRequestEntityTooLarge) {
				return re.JSON(http.StatusRequestEntityTooLarge, map[string]string{"error": "request exceeds 256 MiB"})
			}
			return re.JSON(http.StatusBadRequest, map[string]string{"error": "expected multipart/form-data with one file field named file"})
		}
		form := re.Request.MultipartForm
		if len(form.Value["hash"]) > 1 {
			return re.JSON(http.StatusBadRequest, map[string]string{"error": "provide at most one hash field"})
		}
		expectedHash := ""
		if len(form.Value["hash"]) == 1 {
			expectedHash = strings.TrimSpace(form.Value["hash"][0])
		}
		if len(form.File) != 1 || len(form.File["file"]) != 1 {
			return re.JSON(http.StatusBadRequest, map[string]string{"error": "provide exactly one file field named file"})
		}
		source, header, err := re.Request.FormFile("file")
		if err != nil {
			return err
		}
		defer source.Close()
		key := re.Request.URL.Query().Get("key")
		if key == "" {
			key = "tests/" + path.Base(strings.ReplaceAll(header.Filename, "\\", "/"))
		}
		if len(key) > 1024 || !strings.HasPrefix(key, "tests/") || path.Clean(key) != key || key == "tests/" || strings.ContainsRune(key, '\x00') || strings.Contains(key, "\\") {
			return re.JSON(http.StatusBadRequest, map[string]string{"error": "key must be a clean object path under tests/, e.g. tests/video.mp4"})
		}
		staged, err := os.CreateTemp(stagingDir, "upload-*")
		if err != nil {
			return err
		}
		defer os.Remove(staged.Name())
		defer staged.Close()
		size, err := io.Copy(staged, source)
		if err != nil {
			return err
		}
		if err := staged.Close(); err != nil {
			return err
		}
		result, err := uploader.UploadFileUnique(re.Request.Context(), staged.Name(), key, expectedHash, nil)
		if err != nil {
			if errors.Is(err, s3.ErrInvalidHash) || errors.Is(err, s3.ErrHashMismatch) {
				return re.JSON(http.StatusBadRequest, map[string]string{"error": err.Error()})
			}
			if errors.Is(err, telegramstorage.ErrUploadBusy) {
				return re.JSON(http.StatusConflict, map[string]string{"error": err.Error()})
			}
			if errors.Is(err, telegramstorage.ErrTooLarge) {
				return re.JSON(http.StatusRequestEntityTooLarge, map[string]string{"error": err.Error()})
			}
			re.App.Logger().Error("test upload failed", "key", key, "error", err)
			return re.JSON(http.StatusBadGateway, map[string]any{
				"error": "Upload failed; resend the same file and key to resume, or inspect server logs",
				"key":   key,
			})
		}

		probe, err := os.Open(staged.Name())
		if err != nil {
			return err
		}
		var sniff [512]byte
		n, _ := probe.Read(sniff[:])
		probe.Close()
		contentType := http.DetectContentType(sniff[:n])
		if contentType == "application/octet-stream" {
			if inferred := mime.TypeByExtension(path.Ext(header.Filename)); inferred != "" {
				contentType = inferred
			}
		}
		record, err := saveUploadedMedia(re.App, result, path.Base(strings.ReplaceAll(header.Filename, "\\", "/")), contentType, size)
		if err != nil {
			re.App.Logger().Error("save uploaded media failed", "key", result.Key, "error", err)
			return re.JSON(500, map[string]any{"error": "file is stored but database save failed; retry the upload to link the record", "key": result.Key})
		}
		mediaID := record.Id
		streamURL := "/api/media/" + mediaID + "/stream"
		status := "success"
		if result.Duplicate {
			status = "duplicate"
		}
		return re.JSON(http.StatusOK, map[string]any{
			"media_id": mediaID, "stream_url": streamURL, "thumbnail_url": "/api/media/" + mediaID + "/thumb", "view_url": testMediaPrefix + "/view?id=" + mediaID, "backend": result.Backend, "bucket": result.Bucket, "key": result.Key, "etag": result.ETag,
			"size_bytes": size, "status": status, "hash": result.Hash, "duplicate": result.Duplicate,
		})
	}
}

func newStorageClient(config s3.Config) (fileUploader, error) {
	switch config.StorageBackend {
	case "telegram":
		return telegramstorage.New(config)
	case "", "s3":
		return s3.New(config)
	default:
		return nil, fmt.Errorf("unknown storage_backend %q", config.StorageBackend)
	}
}
