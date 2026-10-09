package routes

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/filesystem"
	"github.com/pocketbase/pocketbase/tools/router"
)

const thumbnailTimeout = 45 * time.Second
const thumbnailRetryDelay = 5 * time.Minute

type thumbnailService struct {
	resolve  func(string) (objectStreamer, error)
	slot     chan struct{}
	mutex    sync.Mutex
	failed   map[string]time.Time
	generate func(context.Context, core.App, *core.Record, objectStreamer) ([]byte, error)
}

func newThumbnailService(resolve func(string) (objectStreamer, error)) *thumbnailService {
	return &thumbnailService{resolve: resolve, slot: make(chan struct{}, 1), failed: map[string]time.Time{}, generate: generateThumbnail}
}

func thumbnailSource(record *core.Record) string {
	var values []string
	for _, field := range []string{"file_hash", "storage_backend", "storage_bucket", "storage_key", "storage_etag", "mime_type", "upload_status"} {
		values = append(values, record.GetString(field))
	}
	return strings.Join(values, "\x00")
}

func (s *thumbnailService) handler() func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		e.Response.Header().Set("Cache-Control", "private, no-cache")
		e.Response.Header().Set("Referrer-Policy", "no-referrer")
		e.Response.Header().Set("Access-Control-Expose-Headers", "Retry-After, ETag, Last-Modified")
		e.Response.Header().Add("Vary", "Authorization")
		e.Response.Header().Add("Vary", "X-S3-Test-Token")
		e.Response.Header().Add("Vary", "Cookie")
		record, err := e.App.FindRecordById("media_item", e.Request.PathValue("id"))
		if errors.Is(err, sql.ErrNoRows) {
			return e.NotFoundError("Media not found", nil)
		}
		if err != nil {
			return err
		}
		fs, err := e.App.NewFilesystem()
		if err != nil {
			return err
		}
		defer fs.Close()
		fs.SetContext(e.Request.Context())
		serveCached := func(record *core.Record) (bool, error) {
			name := record.GetString("thumbs")
			if name == "" {
				return false, nil
			}
			key := path.Join(record.BaseFilesPath(), name)
			exists, err := fs.Exists(key)
			if err != nil || !exists {
				return false, err
			}
			e.Response.Header().Set("ETag", fmt.Sprintf("%q", name))
			return true, fs.Serve(e.Response, e.Request, key, name)
		}
		if found, err := serveCached(record); found || err != nil {
			return err
		}
		if e.Request.Method == http.MethodHead {
			return e.NotFoundError("Thumbnail has not been generated", nil)
		}
		mimeType := record.GetString("mime_type")
		if !strings.HasPrefix(mimeType, "image/") && !strings.HasPrefix(mimeType, "video/") {
			return e.JSON(415, map[string]string{"error": "Thumbnails are available for images and videos"})
		}
		if record.GetString("upload_status") != "success" || record.GetString("storage_backend") == "" || record.GetString("storage_bucket") == "" || record.GetString("storage_key") == "" {
			return e.JSON(409, map[string]string{"error": "Media has no completed storage mapping"})
		}
		// Cached files bypass the worker slot; uncached requests never accumulate a queue.
		select {
		case s.slot <- struct{}{}:
			defer func() { <-s.slot }()
		default:
			e.Response.Header().Set("Retry-After", "2")
			return e.JSON(503, map[string]string{"error": "Thumbnail generator is busy; retry shortly"})
		}
		// Re-read after acquiring the slot: another request may have just saved the file.
		record, err = e.App.FindRecordById("media_item", record.Id)
		if err != nil {
			return err
		}
		if found, err := serveCached(record); found || err != nil {
			return err
		}
		source := thumbnailSource(record)
		failureKey := record.Id + "\x00" + source
		s.mutex.Lock()
		for key, until := range s.failed {
			if !time.Now().Before(until) {
				delete(s.failed, key)
			}
		}
		until := s.failed[failureKey]
		s.mutex.Unlock()
		if time.Now().Before(until) {
			e.Response.Header().Set("Retry-After", fmt.Sprint(int(time.Until(until).Seconds())+1))
			return e.JSON(503, map[string]string{"error": "Thumbnail generation previously failed; retry later"})
		}
		client, err := s.resolve(record.GetString("storage_backend"))
		if err != nil {
			return e.JSON(503, map[string]string{"error": "Media storage is not configured"})
		}
		ctx, cancel := context.WithTimeout(e.Request.Context(), thumbnailTimeout)
		defer cancel()
		data, err := s.generate(ctx, e.App, record, client)
		if err != nil {
			if e.Request.Context().Err() == nil {
				s.mutex.Lock()
				if len(s.failed) >= 256 {
					clear(s.failed)
				}
				s.failed[failureKey] = time.Now().Add(thumbnailRetryDelay)
				s.mutex.Unlock()
			}
			e.App.Logger().Error("thumbnail generation failed", "record", record.Id, "error", err)
			e.Response.Header().Set("Retry-After", "300")
			return e.JSON(503, map[string]string{"error": "Could not generate thumbnail; original media is still available"})
		}
		file, err := filesystem.NewFileFromBytes(data, "thumb.jpg")
		if err != nil {
			return err
		}
		// Do not write stale metadata or a thumbnail for a concurrently replaced source.
		err = e.App.RunInTransaction(func(tx core.App) error {
			fresh, err := tx.FindRecordById("media_item", record.Id)
			if err != nil {
				return err
			}
			if thumbnailSource(fresh) != source {
				return errors.New("thumbnail source changed")
			}
			fresh.Set("thumbs", file)
			if err := tx.Save(fresh); err != nil {
				return err
			}
			record = fresh
			return nil
		})
		if err != nil {
			e.App.Logger().Error("save thumbnail failed", "record", record.Id, "error", err)
			return e.JSON(409, map[string]string{"error": "Media changed or thumbnail could not be saved; retry"})
		}
		_, err = serveCached(record)
		return err
	}
}

// FFmpeg seeks through a temporary loopback range server. No full video download,
// merged chunk file, public URL, or credentials are passed to the subprocess.
func generateThumbnail(ctx context.Context, app core.App, record *core.Record, client objectStreamer) ([]byte, error) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		return nil, fmt.Errorf("FFmpeg must be installed: %w", err)
	}
	bucket, key := record.GetString("storage_bucket"), record.GetString("storage_key")
	stat, err := client.StatObject(ctx, bucket, key)
	if err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	var secret [24]byte
	if _, err := rand.Read(secret[:]); err != nil {
		listener.Close()
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sourcePath := "/" + hex.EncodeToString(secret[:])
	server := &http.Server{ReadHeaderTimeout: 5 * time.Second, BaseContext: func(net.Listener) context.Context { return ctx }, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != sourcePath || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
			http.NotFound(w, r)
			return
		}
		e := &core.RequestEvent{App: app, Event: router.Event{Request: r, Response: w}}
		if err := serveMedia(e, client, bucket, key, record.GetString("original_relative_path"), record.GetString("mime_type"), stat); err != nil {
			http.Error(w, "Source unavailable", 502)
		}
	})}
	go server.Serve(listener)
	defer server.Close()
	dir, err := os.MkdirTemp("", "media-thumbnail-*")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	output := filepath.Join(dir, "thumb.jpg")
	args := []string{"-hide_banner", "-loglevel", "error", "-nostdin", "-y", "-max_alloc", "67108864", "-threads", "1", "-filter_threads", "1", "-rw_timeout", "15000000",
		"-probesize", "1048576", "-analyzeduration", "1000000", "-protocol_whitelist", "http,tcp", "-format_whitelist", "image2,jpeg_pipe,png_pipe,webp_pipe,bmp_pipe,tiff_pipe,gif,mov,matroska,webm,avi,mpegts,mpeg,flv,ogg,asf,hevc,h264,av1",
		"-i", "http://" + listener.Addr().String() + sourcePath, "-map", "0:v:0", "-frames:v", "1", "-an", "-sn", "-dn",
		"-vf", "scale=w='min(320,iw)':h='min(320,ih)':force_original_aspect_ratio=decrease", "-threads", "1", "-q:v", "5", "-update", "1", output}
	command := exec.CommandContext(ctx, ffmpeg, args...)
	command.Stdout = io.Discard
	// Keep decoder errors bounded, even for corrupt files.
	command.Stderr = &boundedThumbnailLog{}
	if err := command.Run(); err != nil {
		return nil, fmt.Errorf("FFmpeg: %w: %s", err, command.Stderr)
	}
	cancel()
	file, err := os.Open(output)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(data) > 1<<20 {
		return nil, errors.New("thumbnail exceeds 1 MiB")
	}
	dimensions, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != "jpeg" || dimensions.Width > 320 || dimensions.Height > 320 {
		return nil, errors.New("invalid generated thumbnail")
	}
	return data, nil
}

type boundedThumbnailLog struct{ data []byte }

func (b *boundedThumbnailLog) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 2048 - len(b.data); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		b.data = append(b.data, p...)
	}
	return n, nil
}
func (b *boundedThumbnailLog) String() string { return string(b.data) }
