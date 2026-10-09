package routes

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/netip"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/aahl/tgnas/store"
	"github.com/aws/smithy-go"
	"github.com/pocketbase/pocketbase/core"
	"go-test/s3"
)

type objectStreamer interface {
	StatObject(context.Context, string, string) (s3.StreamInfo, error)
	OpenObject(context.Context, string, string, *s3.ByteRange) (io.ReadCloser, error)
}

type storageRegistry struct {
	mutex     sync.Mutex
	lifecycle sync.Mutex
	config    s3.Config
	clients   map[string]fileUploader
}

func (r *storageRegistry) get(backend string) (fileUploader, error) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if backend == "" {
		backend = "s3"
	}
	if client := r.clients[backend]; client != nil {
		return client, nil
	}
	config := r.config
	config.StorageBackend = backend
	client, err := newStorageClient(config)
	if err != nil {
		return nil, err
	}
	r.clients[backend] = client
	return client, nil
}
func (r *storageRegistry) close() error {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	var result error
	for _, client := range r.clients {
		if closer, ok := client.(io.Closer); ok {
			result = errors.Join(result, closer.Close())
		}
	}
	r.clients = map[string]fileUploader{}
	return result
}

// Streams use the media collection's view rule, or the test header for test objects.
func mediaStreamHandler(resolve func(string) (objectStreamer, error), testToken string) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		record, err := e.App.FindRecordById("media_item", e.Request.PathValue("id"))
		if errors.Is(err, sql.ErrNoRows) {
			return e.NotFoundError("Media not found", nil)
		}
		if err != nil {
			return err
		}
		if record.GetInt("trashed_at") > 0 {
			return e.NotFoundError("Media is in trash", nil)
		}
		if err := authorizeMedia(e, record, testToken); err != nil {
			return err
		}
		backend, bucket, key := record.GetString("storage_backend"), record.GetString("storage_bucket"), record.GetString("storage_key")
		if record.GetString("upload_status") != "success" || backend == "" || bucket == "" || key == "" {
			return e.JSON(409, map[string]string{"error": "media has no completed storage mapping; upload it through the upload endpoint first"})
		}
		client, err := resolve(backend)
		if err != nil {
			e.App.Logger().Error("resolve media storage", "error", err)
			return e.JSON(503, map[string]string{"error": "media storage is not configured"})
		}
		stat, err := client.StatObject(e.Request.Context(), bucket, key)
		if err != nil {
			var apiError smithy.APIError
			if errors.Is(err, store.ErrNoSuchKey) || (errors.As(err, &apiError) && (apiError.ErrorCode() == "NotFound" || apiError.ErrorCode() == "NoSuchKey")) {
				return e.NotFoundError("Stored media not found", nil)
			}
			e.App.Logger().Error("media storage metadata failed", "record", record.Id, "error", err)
			return e.JSON(502, map[string]string{"error": "media storage is unavailable"})
		}
		return serveMedia(e, client, bucket, key, record.GetString("original_relative_path"), record.GetString("mime_type"), stat)
	}
}

// Shared by streaming and thumbnail routes so private media has identical access rules.
func authorizeMedia(e *core.RequestEvent, record *core.Record, testToken string) error {
	testAccess := testToken != "" && strings.HasPrefix(record.GetString("storage_key"), "tests/") && subtle.ConstantTimeCompare([]byte(e.Request.Header.Get("X-S3-Test-Token")), []byte(testToken)) == 1
	if !testAccess {
		info, err := e.RequestInfo()
		if err != nil {
			return err
		}
		accessInfo := *info
		if token := e.Request.URL.Query().Get("token"); token != "" && info.Auth == nil {
			auth, _ := e.App.FindAuthRecordByToken(token, core.TokenTypeFile)
			if auth != nil && auth.IsSuperuser() && len(e.App.Settings().SuperuserIPs) > 0 && !streamIPAllowed(e.RealIP(), e.App.Settings().SuperuserIPs) {
				auth = nil
			}
			accessInfo.Auth = auth
			accessInfo.Context = core.RequestInfoContextProtectedFile
		}
		allowed, err := e.App.CanAccessRecord(record, &accessInfo, record.Collection().ViewRule)
		if err != nil {
			return err
		}
		if !allowed {
			return e.NotFoundError("Media not found", nil)
		}
	}
	return nil
}

func serveMedia(e *core.RequestEvent, client objectStreamer, bucket, key, filename, contentType string, stat s3.StreamInfo) error {
	if stat.Size < 0 {
		return e.InternalServerError("Invalid stored media size", nil)
	}
	headers := e.Response.Header()
	headers.Set("Accept-Ranges", "bytes")
	headers.Set("Access-Control-Expose-Headers", "Accept-Ranges, Content-Range, Content-Length, ETag, Last-Modified")
	headers.Set("Referrer-Policy", "no-referrer")
	headers.Set("Content-Security-Policy", "sandbox")
	headers.Set("Cache-Control", "private, no-cache")
	headers.Add("Vary", "Authorization")
	headers.Add("Vary", "X-S3-Test-Token")
	etag := strings.Trim(stat.ETag, "\"")
	if etag != "" {
		etag = "\"" + etag + "\""
		headers.Set("ETag", etag)
	}
	modified := stat.Modified.UTC().Truncate(time.Second)
	if !modified.IsZero() {
		headers.Set("Last-Modified", modified.Format(http.TimeFormat))
	}
	if value := e.Request.Header.Get("If-None-Match"); value != "" {
		for _, candidate := range strings.Split(value, ",") {
			candidate = strings.TrimSpace(candidate)
			if candidate == "*" || (etag != "" && strings.TrimPrefix(candidate, "W/") == etag) {
				e.Response.WriteHeader(http.StatusNotModified)
				return nil
			}
		}
	} else if value := e.Request.Header.Get("If-Modified-Since"); value != "" && !modified.IsZero() {
		if when, err := http.ParseTime(value); err == nil && !modified.After(when) {
			e.Response.WriteHeader(http.StatusNotModified)
			return nil
		}
	}
	var byteRange *s3.ByteRange
	status, length := http.StatusOK, stat.Size
	rangeHeader := e.Request.Header.Get("Range")
	if e.Request.Method == http.MethodGet && rangeHeader != "" {
		honor := true
		if value := e.Request.Header.Get("If-Range"); value != "" {
			honor = etag != "" && value == etag
			if when, err := http.ParseTime(value); err == nil {
				honor = !modified.IsZero() && !modified.After(when)
			}
		}
		if honor {
			parsed, err := store.ParseRange(rangeHeader, stat.Size)
			if err != nil {
				headers.Set("Content-Range", fmt.Sprintf("bytes */%d", stat.Size))
				e.Response.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return nil
			}
			byteRange = &s3.ByteRange{Start: parsed.Start, End: parsed.End}
			status, length = http.StatusPartialContent, parsed.Length()
			headers.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", parsed.Start, parsed.End, stat.Size))
		}
	}
	if contentType == "" {
		contentType = stat.ContentType
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	filename = path.Base(strings.ReplaceAll(filename, "\\", "/"))
	if filename == "." || filename == "/" || filename == "" {
		filename = path.Base(key)
	}
	// Open before committing headers so setup failures still return a clean error response.
	var reader io.ReadCloser
	if e.Request.Method != http.MethodHead && length > 0 {
		var err error
		reader, err = client.OpenObject(e.Request.Context(), bucket, key, byteRange)
		if err != nil {
			headers.Del("Content-Range")
			return e.JSON(502, map[string]string{"error": "could not open stored media"})
		}
		defer reader.Close()
	}
	headers.Set("Content-Type", contentType)
	headers.Set("Content-Disposition", mime.FormatMediaType("inline", map[string]string{"filename": filename}))
	headers.Set("Content-Length", fmt.Sprint(length))
	e.Response.WriteHeader(status)
	if reader != nil {
		if _, err := io.CopyN(e.Response, reader, length); err != nil && e.Request.Context().Err() == nil {
			e.App.Logger().Error("media stream interrupted", "key", key, "error", err)
		}
	}
	return nil
}

func streamIPAllowed(ip string, allowed []string) bool {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	for _, value := range allowed {
		if prefix, err := netip.ParsePrefix(value); err == nil && prefix.Contains(addr) {
			return true
		}
		if candidate, err := netip.ParseAddr(value); err == nil && candidate == addr {
			return true
		}
	}
	return false
}
