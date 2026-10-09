package routes

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
	"go-test/s3"
)

// Model a split object without a merged intermediate file.
type thumbnailObject struct {
	data  []byte
	opens atomic.Int32
}

func (s *thumbnailObject) StatObject(context.Context, string, string) (s3.StreamInfo, error) {
	return s3.StreamInfo{Size: int64(len(s.data))}, nil
}
func (s *thumbnailObject) OpenObject(_ context.Context, _, _ string, r *s3.ByteRange) (io.ReadCloser, error) {
	s.opens.Add(1)
	start, end := int64(0), int64(len(s.data))
	if r != nil {
		start, end = r.Start, r.End+1
	}
	var chunks []io.Reader
	for start < end {
		next := min(end, ((start/512)+1)*512)
		chunks = append(chunks, bytes.NewReader(s.data[start:next]))
		start = next
	}
	return io.NopCloser(io.MultiReader(chunks...)), nil
}
func thumbnailEvent(app core.App, id, method, token string) (*core.RequestEvent, *httptest.ResponseRecorder) {
	request := httptest.NewRequest(method, "/api/media/"+id+"/thumb", nil)
	request.SetPathValue("id", id)
	request.Header.Set("X-S3-Test-Token", token)
	response := httptest.NewRecorder()
	return &core.RequestEvent{App: app, Event: router.Event{Request: request, Response: response}}, response
}
func thumbnailPNG(t *testing.T) []byte {
	t.Helper()
	im := image.NewRGBA(image.Rect(0, 0, 640, 360))
	for y := 0; y < 360; y++ {
		for x := 0; x < 640; x++ {
			im.Set(x, y, color.RGBA{R: 200, A: 255})
		}
	}
	var data bytes.Buffer
	if err := png.Encode(&data, im); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}
func TestThumbnailFFmpegImageAndSplitVideoCache(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("FFmpeg not installed")
	}
	pngData := thumbnailPNG(t)
	videoPath := path.Join(t.TempDir(), "video.mp4")
	command := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "color=c=red:s=640x360:d=1", "-threads", "1", "-c:v", "mpeg4", "-y", videoPath)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("video fixture: %s: %v", output, err)
	}
	videoData, err := os.ReadFile(videoPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, mime string
		data       []byte
	}{{"image.png", "image/png", pngData}, {"video.mp4", "video/mp4", videoData}} {
		t.Run(tc.name, func(t *testing.T) {
			app := mediaTestApp(t)
			record, err := saveUploadedMedia(app, s3.Result{Backend: "telegram", Bucket: "telegram", Key: "tests/" + tc.name, Hash: strings.Repeat("a", 64)}, tc.name, tc.mime, int64(len(tc.data)))
			if err != nil {
				t.Fatal(err)
			}
			source := &thumbnailObject{data: tc.data}
			service := newThumbnailService(func(string) (objectStreamer, error) { return source, nil })
			service.generate = func(ctx context.Context, app core.App, record *core.Record, client objectStreamer) ([]byte, error) {
				data, err := generateThumbnail(ctx, app, record, client)
				if err != nil {
					t.Logf("generator error: %v", err)
				}
				return data, err
			}
			handler := service.handler()
			event, response := thumbnailEvent(app, record.Id, "GET", "secret")
			if err := handler(event); err != nil {
				t.Fatal(err)
			}
			if response.Code != 200 {
				t.Fatalf("status %d: %s", response.Code, response.Body.String())
			}
			dimensions, format, err := image.DecodeConfig(bytes.NewReader(response.Body.Bytes()))
			if err != nil || format != "jpeg" || dimensions.Width != 320 || dimensions.Height != 180 {
				t.Fatalf("dimensions=%v format=%s err=%v", dimensions, format, err)
			}
			cached, err := app.FindRecordById("media_item", record.Id)
			if err != nil {
				t.Fatal(err)
			}
			name := cached.GetString("thumbs")
			if name == "" {
				t.Fatal("thumbnail not persisted")
			}
			opens := source.opens.Load()
			event, response = thumbnailEvent(app, record.Id, "GET", "secret")
			event.Request.Header.Set("If-None-Match", "\""+name+"\"")
			if err := handler(event); err != nil {
				t.Fatal(err)
			}
			if response.Code != 304 || source.opens.Load() != opens {
				t.Fatal("cached request accessed original or ignored ETag", response.Code)
			}
			event, _ = thumbnailEvent(app, record.Id, "GET", "")
			if err := handler(event); err != nil {
				t.Fatal("thumbnail still requires a token", err)
			}
			fs, err := app.NewFilesystem()
			if err != nil {
				t.Fatal(err)
			}
			if err := fs.Delete(path.Join(cached.BaseFilesPath(), name)); err != nil {
				t.Fatal(err)
			}
			fs.Close()
			event, response = thumbnailEvent(app, record.Id, "GET", "secret")
			if err := handler(event); err != nil {
				t.Fatal(err)
			}
			if response.Code != 200 || source.opens.Load() <= opens {
				t.Fatal("missing cached file was not repaired")
			}
			cached, err = app.FindRecordById("media_item", record.Id)
			if err != nil {
				t.Fatal(err)
			}
			name = cached.GetString("thumbs")
			opens = source.opens.Load()
			cached.Set("original_relative_path", "renamed"+tc.name)
			if err := app.Save(cached); err != nil {
				t.Fatal(err)
			}
			if cached.GetString("thumbs") != name {
				t.Fatal("metadata edit cleared thumbnail")
			}
			cached.Set("storage_etag", "replacement")
			if err := app.Save(cached); err != nil {
				t.Fatal(err)
			}
			if cached.GetString("thumbs") != "" {
				t.Fatal("source change retained stale thumbnail")
			}
			event, response = thumbnailEvent(app, record.Id, "GET", "secret")
			if err := handler(event); err != nil {
				t.Fatal(err)
			}
			if response.Code != 200 || source.opens.Load() <= opens {
				t.Fatal("source change did not regenerate", response.Code)
			}
		})
	}
}

func TestThumbnailBusyFailureCooldownAndHead(t *testing.T) {
	app := mediaTestApp(t)
	record, err := saveUploadedMedia(app, s3.Result{Backend: "telegram", Bucket: "telegram", Key: "tests/broken.mp4", Hash: strings.Repeat("b", 64)}, "broken.mp4", "video/mp4", 10)
	if err != nil {
		t.Fatal(err)
	}
	service := newThumbnailService(func(string) (objectStreamer, error) { return &thumbnailObject{data: []byte("bad")}, nil })
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	service.generate = func(context.Context, core.App, *core.Record, objectStreamer) ([]byte, error) {
		calls.Add(1)
		close(entered)
		<-release
		return nil, errors.New("corrupt source")
	}
	handler := service.handler()
	event, _ := thumbnailEvent(app, record.Id, "HEAD", "secret")
	if err := handler(event); err == nil || calls.Load() != 0 {
		t.Fatal("HEAD generated missing thumbnail")
	}
	var group sync.WaitGroup
	group.Add(1)
	go func() {
		defer group.Done()
		event, response := thumbnailEvent(app, record.Id, "GET", "secret")
		if err := handler(event); err != nil || response.Code != 503 {
			t.Errorf("failure status %d: %v", response.Code, err)
		}
	}()
	<-entered
	event, response := thumbnailEvent(app, record.Id, "GET", "secret")
	if err := handler(event); err != nil || response.Code != http.StatusServiceUnavailable || response.Header().Get("Retry-After") != "2" {
		t.Fatal("busy request queued", response.Code, err)
	}
	close(release)
	group.Wait()
	event, response = thumbnailEvent(app, record.Id, "GET", "secret")
	if err := handler(event); err != nil || response.Code != 503 || calls.Load() != 1 {
		t.Fatal("failed source retried during cooldown", response.Code, err)
	}
}
