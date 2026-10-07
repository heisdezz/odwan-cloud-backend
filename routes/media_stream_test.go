package routes

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
	appinit "go-test/init"
	"go-test/s3"
)

func mediaTestApp(t *testing.T) *pocketbase.PocketBase {
	t.Helper()
	app := pocketbase.NewWithConfig(pocketbase.Config{DefaultDataDir: t.TempDir()})
	app.OnBootstrap().BindFunc(appinit.Initialize)
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.ResetBootstrapState() })
	return app
}

type streamStub struct {
	data     []byte
	calls    int
	gotRange *s3.ByteRange
	fail     bool
	closed   bool
}
type trackedReader struct {
	io.Reader
	close func()
}

func (r *trackedReader) Close() error { r.close(); return nil }
func (s *streamStub) StatObject(context.Context, string, string) (s3.StreamInfo, error) {
	return s3.StreamInfo{Size: int64(len(s.data)), ContentType: "video/mp4", ETag: "test-etag", Modified: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}, nil
}
func (s *streamStub) OpenObject(ctx context.Context, bucket, key string, r *s3.ByteRange) (io.ReadCloser, error) {
	s.calls++
	s.gotRange = r
	if s.fail {
		return nil, errors.New("unavailable")
	}
	data := s.data
	if r != nil {
		data = data[r.Start : r.End+1]
	}
	return &trackedReader{Reader: bytes.NewReader(data), close: func() { s.closed = true }}, nil
}

func TestMediaStreamingRangesAndConditions(t *testing.T) {
	app := mediaTestApp(t)
	record, err := saveUploadedMedia(app, s3.Result{Backend: "telegram", Bucket: "telegram", Key: "tests/video.mp4", Hash: strings.Repeat("a", 64), ETag: "test-etag"}, "video.mp4", "video/mp4", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, method, rangeHeader, ifRange, ifNone string
		status                                     int
		body, contentRange                         string
		opens                                      int
	}{
		{name: "full", method: "GET", status: 200, body: "0123456789", opens: 1},
		{name: "cross chunk range", method: "GET", rangeHeader: "bytes=3-7", status: 206, body: "34567", contentRange: "bytes 3-7/10", opens: 1},
		{name: "open ended", method: "GET", rangeHeader: "bytes=8-", status: 206, body: "89", contentRange: "bytes 8-9/10", opens: 1},
		{name: "suffix", method: "GET", rangeHeader: "bytes=-3", status: 206, body: "789", contentRange: "bytes 7-9/10", opens: 1},
		{name: "clamp", method: "GET", rangeHeader: "bytes=8-100", status: 206, body: "89", contentRange: "bytes 8-9/10", opens: 1},
		{name: "invalid", method: "GET", rangeHeader: "bytes=10-", status: 416, contentRange: "bytes */10"},
		{name: "multiple", method: "GET", rangeHeader: "bytes=0-1,5-6", status: 416, contentRange: "bytes */10"},
		{name: "head", method: "HEAD", rangeHeader: "bytes=3-7", status: 200},
		{name: "cached", method: "GET", ifNone: "W/\"test-etag\"", status: 304},
		{name: "if range match", method: "GET", rangeHeader: "bytes=2-3", ifRange: "\"test-etag\"", status: 206, body: "23", contentRange: "bytes 2-3/10", opens: 1},
		{name: "if range mismatch", method: "GET", rangeHeader: "bytes=2-3", ifRange: "\"old\"", status: 200, body: "0123456789", opens: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &streamStub{data: []byte("0123456789")}
			req := httptest.NewRequest(tc.method, "/api/media/"+record.Id+"/stream", nil)
			req.SetPathValue("id", record.Id)
			req.Header.Set("X-S3-Test-Token", "secret")
			req.Header.Set("Range", tc.rangeHeader)
			req.Header.Set("If-Range", tc.ifRange)
			req.Header.Set("If-None-Match", tc.ifNone)
			response := httptest.NewRecorder()
			event := &core.RequestEvent{App: app, Event: router.Event{Request: req, Response: response}}
			err := mediaStreamHandler(func(string) (objectStreamer, error) { return stub, nil }, "secret")(event)
			if err != nil {
				t.Fatal(err)
			}
			if response.Code != tc.status || response.Body.String() != tc.body || response.Header().Get("Content-Range") != tc.contentRange {
				t.Fatalf("status=%d body=%q headers=%v", response.Code, response.Body.String(), response.Header())
			}
			if stub.calls != tc.opens || (tc.opens > 0 && !stub.closed) {
				t.Fatalf("opens=%d closed=%v", stub.calls, stub.closed)
			}
			if tc.status == 206 && response.Header().Get("Content-Length") != "" && response.Header().Get("Content-Type") != "video/mp4" {
				t.Fatal("wrong content type")
			}
		})
	}
}

func TestMediaStreamingAccessRules(t *testing.T) {
	app := mediaTestApp(t)
	record, err := saveUploadedMedia(app, s3.Result{Backend: "telegram", Bucket: "telegram", Key: "tests/private.mp4", Hash: strings.Repeat("b", 64)}, "private.mp4", "video/mp4", 10)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	handler := mediaStreamHandler(func(string) (objectStreamer, error) { calls++; return &streamStub{data: []byte("0123456789")}, nil }, "secret")
	request := func(token string) *core.RequestEvent {
		req := httptest.NewRequest("GET", "/api/media/"+record.Id+"/stream", nil)
		req.SetPathValue("id", record.Id)
		req.Header.Set("X-S3-Test-Token", token)
		return &core.RequestEvent{App: app, Event: router.Event{Request: req, Response: httptest.NewRecorder()}}
	}
	if err := handler(request("wrong")); err == nil {
		t.Fatal("locked view rule bypassed")
	}
	if calls != 0 {
		t.Fatal("denied request opened storage")
	}
	collection := record.Collection()
	public := ""
	collection.ViewRule = &public
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}
	if err := handler(request("")); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("public rule denied")
	}
}

func TestMediaRecordDedupAndAlbumCounts(t *testing.T) {
	app := mediaTestApp(t)
	result := s3.Result{Backend: "telegram", Bucket: "telegram", Key: "tests/file.jpg", Hash: strings.Repeat("c", 64)}
	first, err := saveUploadedMedia(app, result, "file.jpg", "image/jpeg", 10)
	if err != nil {
		t.Fatal(err)
	}
	result.Duplicate = true
	second, err := saveUploadedMedia(app, result, "other.jpg", "image/jpeg", 10)
	if err != nil {
		t.Fatal(err)
	}
	if second.Id != first.Id || second.GetString("original_relative_path") != "file.jpg" {
		t.Fatal("duplicate created or renamed record")
	}
	album, err := app.FindRecordById("album", "unsorted")
	if err != nil {
		t.Fatal(err)
	}
	if album.GetInt("media_count") != 1 {
		t.Fatalf("album count %d", album.GetInt("media_count"))
	}
}

func TestMediaStreamAcceptsOnlyFileTokensInURLs(t *testing.T) {
	app := mediaTestApp(t)
	users := core.NewAuthCollection("stream_users")
	if err := app.Save(users); err != nil {
		t.Fatal(err)
	}
	user := core.NewRecord(users)
	user.Set("email", "viewer@example.com")
	user.SetPassword("long-test-password")
	if err := app.Save(user); err != nil {
		t.Fatal(err)
	}
	record, err := saveUploadedMedia(app, s3.Result{Backend: "telegram", Bucket: "telegram", Key: "media/video.mp4", Hash: strings.Repeat("d", 64)}, "video.mp4", "video/mp4", 10)
	if err != nil {
		t.Fatal(err)
	}
	collection := record.Collection()
	rule := "@request.auth.id != ''"
	collection.ViewRule = &rule
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}
	fileToken, err := user.NewFileToken()
	if err != nil {
		t.Fatal(err)
	}
	authToken, err := user.NewAuthToken()
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, token string
		allowed     bool
	}{
		{"file token", fileToken, true}, {"auth token", authToken, false}, {"invalid token", "wrong", false}, {"test header outside tests", "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/api/media/"+record.Id+"/stream?token="+test.token, nil)
			req.SetPathValue("id", record.Id)
			req.Header.Set("X-S3-Test-Token", "secret")
			response := httptest.NewRecorder()
			event := &core.RequestEvent{App: app, Event: router.Event{Request: req, Response: response}}
			stub := &streamStub{data: []byte("0123456789")}
			err := mediaStreamHandler(func(string) (objectStreamer, error) { return stub, nil }, "secret")(event)
			if (err == nil) != test.allowed {
				t.Fatalf("allowed=%v error=%v", test.allowed, err)
			}
			if !test.allowed && stub.calls != 0 {
				t.Fatal("denied request downloaded bytes")
			}
		})
	}
}

func TestEmptyMediaAndOpenFailure(t *testing.T) {
	app := mediaTestApp(t)
	for _, test := range []struct {
		name   string
		data   []byte
		fail   bool
		status int
	}{
		{name: "empty", status: 200}, {name: "upstream failed", data: []byte("abc"), fail: true, status: 502},
	} {
		t.Run(test.name, func(t *testing.T) {
			stub := &streamStub{data: test.data, fail: test.fail}
			stat, _ := stub.StatObject(context.Background(), "bucket", "key")
			response := httptest.NewRecorder()
			event := &core.RequestEvent{App: app, Event: router.Event{Request: httptest.NewRequest("GET", "/", nil), Response: response}}
			if err := serveMedia(event, stub, "bucket", "key", "video.mp4", "video/mp4", stat); err != nil {
				t.Fatal(err)
			}
			if response.Code != test.status {
				t.Fatalf("status=%d", response.Code)
			}
			if !test.fail && (stub.calls != 0 || response.Header().Get("Content-Length") != "0") {
				t.Fatal("empty media opened storage")
			}
		})
	}
}

func TestUploadAdoptsExistingUnmappedMedia(t *testing.T) {
	app := mediaTestApp(t)
	albums, err := app.FindCollectionByNameOrId("album")
	if err != nil {
		t.Fatal(err)
	}
	album := core.NewRecord(albums)
	album.Set("name", "Existing")
	album.Set("relative_path", "existing")
	if err := app.Save(album); err != nil {
		t.Fatal(err)
	}
	collection, err := app.FindCollectionByNameOrId("media_item")
	if err != nil {
		t.Fatal(err)
	}
	existing := core.NewRecord(collection)
	existing.Set("file_hash", strings.Repeat("e", 64))
	existing.Set("original_relative_path", "hdd/video.mp4")
	existing.Set("current_relative_path", "hdd/video.mp4")
	existing.Set("mime_type", "video/mp4")
	existing.Set("upload_status", "pending")
	existing.Set("album_id", album.Id)
	if err := app.Save(existing); err != nil {
		t.Fatal(err)
	}
	linked, err := saveUploadedMedia(app, s3.Result{Backend: "telegram", Bucket: "telegram", Key: "tests/video.mp4", Hash: existing.GetString("file_hash")}, "video.mp4", "video/mp4", 10)
	if err != nil {
		t.Fatal(err)
	}
	if linked.Id != existing.Id || linked.GetString("album_id") != album.Id || linked.GetString("original_relative_path") != "hdd/video.mp4" {
		t.Fatal("existing library details changed")
	}
	album, err = app.FindRecordById("album", album.Id)
	if err != nil {
		t.Fatal(err)
	}
	if album.GetInt("media_count") != 1 {
		t.Fatal("album count changed")
	}
}
