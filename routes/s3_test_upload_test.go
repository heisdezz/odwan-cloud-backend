package routes

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"go-test/s3"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
)

type uploadStub struct {
	calls     int
	fail      bool
	data      []byte
	key       string
	path      string
	hash      string
	duplicate bool
	hashError error
}

func (u *uploadStub) UploadFileUnique(ctx context.Context, file, key, hash string, progress func(s3.Progress)) (s3.Result, error) {
	u.calls++
	u.hash = hash
	if u.hashError != nil {
		return s3.Result{}, u.hashError
	}
	u.key = key
	u.path = file
	data, err := os.ReadFile(file)
	if err != nil {
		return s3.Result{}, err
	}
	u.data = data
	if u.fail {
		return s3.Result{}, errors.New("interrupted")
	}
	digest := sha256.Sum256(data)
	return s3.Result{Backend: "s3", Bucket: "bucket", Key: key, ETag: "etag", Hash: hex.EncodeToString(digest[:]), Duplicate: u.duplicate}, nil
}

func uploadRequest(t *testing.T, key, token string, filenames ...string) *http.Request {
	t.Helper()
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for _, name := range filenames {
		field, err := writer.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := field.Write([]byte("test file contents")); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/test/s3/upload"+key, &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("X-S3-Test-Token", token)
	return request
}

func TestS3TestUploadHandler(t *testing.T) {
	for _, tc := range []struct {
		name, query, token string
		files              []string
		status             int
	}{
		{"success", "?key=tests/custom.bin", "secret", []string{"file.bin"}, 200},
		{"default key", "", "secret", []string{"file.bin"}, 200},
		{"unauthorized", "", "", []string{"file.bin"}, 401},
		{"wrong token", "", "wrong", []string{"file.bin"}, 401},
		{"missing file", "", "secret", nil, 400},
		{"multiple files", "", "secret", []string{"one", "two"}, 400},
		{"outside prefix", "?key=media/file.bin", "secret", []string{"file.bin"}, 400},
		{"traversal", "?key=tests/../private", "secret", []string{"file.bin"}, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			stub := &uploadStub{}
			recorder := httptest.NewRecorder()
			event := &core.RequestEvent{App: mediaTestApp(t), Event: router.Event{Request: uploadRequest(t, tc.query, tc.token, tc.files...), Response: recorder}}
			if err := testUploadHandler(stub, dir, "secret")(event); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != tc.status {
				t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
			}
			if tc.status == 200 {
				if stub.calls != 1 || string(stub.data) != "test file contents" {
					t.Fatal("file did not reach uploader")
				}
				want := "tests/file.bin"
				if tc.query != "" {
					want = "tests/custom.bin"
				}
				if stub.key != want {
					t.Fatalf("wrong key: %s", stub.key)
				}
			} else if stub.calls != 0 {
				t.Fatal("invalid request reached uploader")
			}
			files, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(files) != 0 {
				t.Fatal("staged file leaked")
			}
		})
	}
}

func TestS3TestUploadFailureAndRetry(t *testing.T) {
	dir := t.TempDir()
	stub := &uploadStub{fail: true}
	handler := testUploadHandler(stub, dir, "secret")
	for _, status := range []int{502, 200} {
		recorder := httptest.NewRecorder()
		event := &core.RequestEvent{App: mediaTestApp(t), Event: router.Event{Request: uploadRequest(t, "?key=tests/retry.bin", "secret", "file.bin"), Response: recorder}}
		if err := handler(event); err != nil {
			t.Fatal(err)
		}
		if recorder.Code != status {
			t.Fatalf("status %d: %s", recorder.Code, recorder.Body.String())
		}
		if _, err := os.Stat(stub.path); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("staged file not cleaned up")
		}
		stub.fail = false
	}
	if stub.calls != 2 {
		t.Fatal("retry did not reach uploader")
	}
}

func TestS3TestUploadMalformedAndOversized(t *testing.T) {
	for _, oversized := range []bool{false, true} {
		request := httptest.NewRequest(http.MethodPost, "/api/test/s3/upload", bytes.NewBufferString("invalid"))
		request.Header.Set("X-S3-Test-Token", "secret")
		request.Header.Set("Content-Type", "multipart/form-data; boundary=test")
		if oversized {
			request.Body = http.MaxBytesReader(httptest.NewRecorder(), request.Body, 1)
		}
		recorder := httptest.NewRecorder()
		event := &core.RequestEvent{App: mediaTestApp(t), Event: router.Event{Request: request, Response: recorder}}
		if err := testUploadHandler(&uploadStub{}, t.TempDir(), "secret")(event); err != nil {
			t.Fatal(err)
		}
		want := 400
		if oversized {
			want = 413
		}
		if recorder.Code != want {
			t.Fatalf("status %d expected %d", recorder.Code, want)
		}
	}
}

func TestS3TestRoutesDisabledByDefault(t *testing.T) {
	app := mediaTestApp(t)
	r := router.NewRouter(func(w http.ResponseWriter, req *http.Request) (*core.RequestEvent, router.EventCleanupFunc) {
		return &core.RequestEvent{App: app, Event: router.Event{Request: req, Response: w}}, nil
	})
	if err := RegisterStorageRoutes(&core.ServeEvent{App: app, Router: r}, s3.Config{}); err != nil {
		t.Fatal(err)
	}
	if r.HasRoute("POST", "/api/test/s3/upload") {
		t.Fatal("test uploads unexpectedly enabled")
	}
	if !r.HasRoute("GET", "/api/media/{id}/stream") {
		t.Fatal("streaming disabled with test uploads")
	}
}

func TestS3TestUploadHashField(t *testing.T) {
	for _, tc := range []struct {
		name      string
		duplicate bool
		hashError error
		status    int
	}{
		{"duplicate", true, nil, 200}, {"invalid hash", false, s3.ErrInvalidHash, 400}, {"mismatched hash", false, s3.ErrHashMismatch, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body bytes.Buffer
			writer := multipart.NewWriter(&body)
			if err := writer.WriteField("hash", "provided-hash"); err != nil {
				t.Fatal(err)
			}
			field, err := writer.CreateFormFile("file", "file.bin")
			if err != nil {
				t.Fatal(err)
			}
			field.Write([]byte("test file contents"))
			writer.Close()
			request := httptest.NewRequest(http.MethodPost, "/api/test/s3/upload", &body)
			request.Header.Set("Content-Type", writer.FormDataContentType())
			request.Header.Set("X-S3-Test-Token", "secret")
			stub := &uploadStub{duplicate: tc.duplicate, hashError: tc.hashError}
			recorder := httptest.NewRecorder()
			event := &core.RequestEvent{App: mediaTestApp(t), Event: router.Event{Request: request, Response: recorder}}
			if err := testUploadHandler(stub, t.TempDir(), "secret")(event); err != nil {
				t.Fatal(err)
			}
			if recorder.Code != tc.status || stub.hash != "provided-hash" {
				t.Fatalf("incorrect hash handling: %s", recorder.Body.String())
			}
			if tc.duplicate {
				var response struct {
					Hash      string
					Duplicate bool
					Status    string
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if len(response.Hash) != 64 || !response.Duplicate || response.Status != "duplicate" {
					t.Fatal("duplicate response missing hash/status")
				}
			}
		})
	}
}
