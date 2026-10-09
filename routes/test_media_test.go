package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
	"go-test/s3"
)

func TestPublicMediaViewerListAndPlayback(t *testing.T) {
	app := mediaTestApp(t)
	uploaded, err := saveUploadedMedia(app, s3.Result{Backend: "telegram", Bucket: "telegram", Key: "tests/video.mp4", Hash: strings.Repeat("a", 64)}, "video.mp4", "video/mp4", 10)
	if err != nil {
		t.Fatal(err)
	}
	library, err := saveUploadedMedia(app, s3.Result{Backend: "telegram", Bucket: "telegram", Key: "library/video.mp4", Hash: strings.Repeat("b", 64)}, "video.mp4", "video/mp4", 10)
	if err != nil {
		t.Fatal(err)
	}
	stub := &streamStub{data: []byte("0123456789")}
	r := router.NewRouter(func(w http.ResponseWriter, req *http.Request) (*core.RequestEvent, router.EventCleanupFunc) {
		return &core.RequestEvent{App: app, Event: router.Event{Request: req, Response: w}}, nil
	})
	registerTestMediaRoutes(&core.ServeEvent{App: app, Router: r}, "", mediaStreamHandler(func(string) (objectStreamer, error) { return stub, nil }, ""), newThumbnailService(func(string) (objectStreamer, error) { return stub, nil }).handler())
	mux, err := r.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, url string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, url, nil)
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, req)
		return response
	}
	page := request("GET", testMediaPrefix+"/view")
	if page.Code != 200 || strings.Contains(page.Body.String(), "{{nonce}}") || strings.Contains(page.Body.String(), "id=\"token\"") || !strings.Contains(page.Header().Get("Content-Security-Policy"), "script-src 'nonce-") {
		t.Fatal("public viewer page invalid")
	}
	list := request("GET", testMediaPrefix)
	var decoded struct {
		Items []map[string]any `json:"items"`
	}
	if list.Code != 200 || json.Unmarshal(list.Body.Bytes(), &decoded) != nil || len(decoded.Items) != 1 || decoded.Items[0]["id"] != uploaded.Id {
		t.Fatal("public listing failed", list.Code, list.Body.String())
	}
	stream := request("GET", testMediaPrefix+"/"+uploaded.Id+"/stream")
	if stream.Code != 200 || stream.Body.String() != "0123456789" {
		t.Fatal("public playback failed", stream.Code)
	}
	if request("GET", testMediaPrefix+"/"+library.Id+"/stream").Code != 404 {
		t.Fatal("test route included non-test object")
	}
	if request("HEAD", testMediaPrefix+"/"+uploaded.Id+"/thumb").Code != 404 {
		t.Fatal("missing thumbnail should not generate on HEAD")
	}
	if request("GET", testMediaPrefix+"?offset=-1").Code != 400 {
		t.Fatal("invalid offset accepted")
	}
	uploaded.Set("trashed_at", 1)
	if err := app.Save(uploaded); err != nil {
		t.Fatal(err)
	}
	list = request("GET", testMediaPrefix)
	if json.Unmarshal(list.Body.Bytes(), &decoded) != nil || len(decoded.Items) != 0 {
		t.Fatal("trash appears in public test list")
	}
	if request("GET", testMediaPrefix+"/"+uploaded.Id+"/stream").Code != 404 {
		t.Fatal("trash stream is visible")
	}
}
