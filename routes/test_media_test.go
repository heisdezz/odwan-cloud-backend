package routes

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
	"go-test/s3"
)

func TestMediaViewerSessionListAndPlayback(t *testing.T) {
	app := mediaTestApp(t)
	const token = "viewer-secret-do-not-render"
	uploaded, err := saveUploadedMedia(app, s3.Result{Backend: "telegram", Bucket: "telegram", Key: "tests/video.mp4", Hash: strings.Repeat("a", 64)}, "video.mp4", "video/mp4", 10)
	if err != nil {
		t.Fatal(err)
	}
	private, err := saveUploadedMedia(app, s3.Result{Backend: "telegram", Bucket: "telegram", Key: "library/private.mp4", Hash: strings.Repeat("b", 64)}, "private.mp4", "video/mp4", 10)
	if err != nil {
		t.Fatal(err)
	}
	stub := &streamStub{data: []byte("0123456789")}
	r := router.NewRouter(func(w http.ResponseWriter, req *http.Request) (*core.RequestEvent, router.EventCleanupFunc) {
		return &core.RequestEvent{App: app, Event: router.Event{Request: req, Response: w}}, nil
	})
	registerTestMediaRoutes(&core.ServeEvent{App: app, Router: r}, token, mediaStreamHandler(func(string) (objectStreamer, error) { return stub, nil }, token))
	mux, err := r.BuildMux()
	if err != nil {
		t.Fatal(err)
	}
	request := func(method, url, header string, cookie *http.Cookie) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, url, nil)
		if header != "" {
			req.Header.Set("X-S3-Test-Token", header)
		}
		if cookie != nil {
			req.AddCookie(cookie)
		}
		response := httptest.NewRecorder()
		mux.ServeHTTP(response, req)
		return response
	}
	page := request("GET", testMediaPrefix+"/view", "", nil)
	if page.Code != 200 || !strings.Contains(page.Body.String(), "<video") && !strings.Contains(page.Body.String(), "createElement('video')") {
		t.Fatal("viewer page missing", page.Code)
	}
	if strings.Contains(page.Body.String(), token) || strings.Contains(page.Body.String(), "{{nonce}}") || !strings.Contains(page.Header().Get("Content-Security-Policy"), "script-src 'nonce-") {
		t.Fatal("invalid page security headers or secret leaked")
	}
	if request("GET", testMediaPrefix, "", nil).Code != 401 {
		t.Fatal("listing is public")
	}
	bad := request("POST", testMediaPrefix+"/session", "wrong", nil)
	if bad.Code != 401 || len(bad.Result().Cookies()) != 0 {
		t.Fatal("invalid token accepted")
	}
	session := request("POST", testMediaPrefix+"/session", token, nil)
	if session.Code != 200 {
		t.Fatal(session.Body.String())
	}
	cookies := session.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatal("missing session cookie")
	}
	cookie := cookies[0]
	if !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode || cookie.Path != testMediaPrefix || strings.Contains(cookie.Value, token) {
		t.Fatalf("unsafe cookie %+v", cookie)
	}
	listing := request("GET", testMediaPrefix, "", cookie)
	if listing.Code != 200 {
		t.Fatal(listing.Body.String())
	}
	var decoded struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(listing.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Items) != 1 || decoded.Items[0]["id"] != uploaded.Id {
		t.Fatalf("items=%v", decoded.Items)
	}
	req := httptest.NewRequest("GET", testMediaPrefix+"/"+uploaded.Id+"/stream", nil)
	req.AddCookie(cookie)
	req.Header.Set("Range", "bytes=2-5")
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, req)
	if response.Code != 206 || response.Body.String() != "2345" || response.Header().Get("Content-Range") != "bytes 2-5/10" {
		t.Fatalf("range response=%d %q", response.Code, response.Body.String())
	}
	if request("GET", testMediaPrefix+"/"+private.Id+"/stream", "", cookie).Code != 404 {
		t.Fatal("viewer grants non-test access")
	}
	expired := &http.Cookie{Name: testMediaCookie, Value: testMediaSessionValue(token, time.Now().Add(-time.Minute).Unix())}
	if request("GET", testMediaPrefix, "", expired).Code != 401 {
		t.Fatal("expired cookie accepted")
	}
	tampered := &http.Cookie{Name: testMediaCookie, Value: cookie.Value + "x"}
	if request("GET", testMediaPrefix, "", tampered).Code != 401 {
		t.Fatal("tampered cookie accepted")
	}
	if request("GET", testMediaPrefix+"?offset=-1", token, nil).Code != 400 {
		t.Fatal("invalid pagination accepted")
	}
	logout := request("DELETE", testMediaPrefix+"/session", "", cookie)
	if logout.Code != 204 || logout.Result().Cookies()[0].MaxAge != -1 {
		t.Fatal("logout failed")
	}
}

func TestViewingCookieDoesNotAuthorizeNormalStream(t *testing.T) {
	app := mediaTestApp(t)
	record, err := saveUploadedMedia(app, s3.Result{Backend: "telegram", Bucket: "telegram", Key: "tests/video.mp4", Hash: strings.Repeat("c", 64)}, "video.mp4", "video/mp4", 10)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("GET", "/api/media/"+record.Id+"/stream", nil)
	request.SetPathValue("id", record.Id)
	request.AddCookie(&http.Cookie{Name: testMediaCookie, Value: testMediaSessionValue("secret", time.Now().Add(time.Hour).Unix())})
	event := &core.RequestEvent{App: app, Event: router.Event{Request: request, Response: httptest.NewRecorder()}}
	err = mediaStreamHandler(func(string) (objectStreamer, error) { t.Fatal("private storage opened"); return nil, io.EOF }, "secret")(event)
	if err == nil {
		t.Fatal("test cookie bypassed main access rules")
	}
}
