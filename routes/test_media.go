package routes

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	_ "embed"
	"encoding/base64"
	"fmt"
	"net/http"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

//go:embed test_media.html
var testMediaPage string

const testMediaCookie = "test_media_session"
const testMediaPrefix = "/api/test/media"

func testMediaSessionValue(token string, expires int64) string {
	value := strconv.FormatInt(expires, 10)
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write([]byte("test-media-view:" + value))
	return value + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
func testMediaAuthorized(request *http.Request, token string) bool {
	if token == "" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(request.Header.Get("X-S3-Test-Token")), []byte(token)) == 1 {
		return true
	}
	cookie, err := request.Cookie(testMediaCookie)
	if err != nil {
		return false
	}
	expiry, _, ok := strings.Cut(cookie.Value, ".")
	if !ok {
		return false
	}
	expires, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil || expires <= time.Now().Unix() {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(testMediaSessionValue(token, expires))) == 1
}
func registerTestMediaRoutes(se *core.ServeEvent, token string, stream, thumbnail func(*core.RequestEvent) error) {
	if token == "" {
		return
	}
	se.Router.GET(testMediaPrefix+"/view", func(e *core.RequestEvent) error {
		var nonceBytes [24]byte
		if _, err := rand.Read(nonceBytes[:]); err != nil {
			return err
		}
		nonce := base64.RawStdEncoding.EncodeToString(nonceBytes[:])
		e.Response.Header().Set("Content-Type", "text/html; charset=utf-8")
		e.Response.Header().Set("Cache-Control", "no-store")
		e.Response.Header().Set("Referrer-Policy", "no-referrer")
		e.Response.Header().Set("Content-Security-Policy", fmt.Sprintf("default-src 'none'; script-src 'nonce-%s'; style-src 'nonce-%s'; connect-src 'self'; media-src 'self'; img-src 'self' blob:; base-uri 'none'; frame-ancestors 'none'; form-action 'self'", nonce, nonce))
		_, err := e.Response.Write([]byte(strings.ReplaceAll(testMediaPage, "{{nonce}}", nonce)))
		return err
	})
	se.Router.POST(testMediaPrefix+"/session", func(e *core.RequestEvent) error {
		if subtle.ConstantTimeCompare([]byte(e.Request.Header.Get("X-S3-Test-Token")), []byte(token)) != 1 {
			return e.JSON(401, map[string]string{"error": "Invalid test token"})
		}
		expires := time.Now().Add(time.Hour)
		http.SetCookie(e.Response, &http.Cookie{Name: testMediaCookie, Value: testMediaSessionValue(token, expires.Unix()), Path: testMediaPrefix, Expires: expires, MaxAge: 3600, HttpOnly: true, Secure: e.Request.TLS != nil, SameSite: http.SameSiteStrictMode})
		e.Response.Header().Set("Cache-Control", "no-store")
		return e.JSON(200, map[string]any{"expires_at": expires.UTC()})
	})
	se.Router.DELETE(testMediaPrefix+"/session", func(e *core.RequestEvent) error {
		http.SetCookie(e.Response, &http.Cookie{Name: testMediaCookie, Path: testMediaPrefix, MaxAge: -1, HttpOnly: true, Secure: e.Request.TLS != nil, SameSite: http.SameSiteStrictMode})
		e.Response.WriteHeader(204)
		return nil
	})
	se.Router.GET(testMediaPrefix, func(e *core.RequestEvent) error {
		e.Response.Header().Set("Cache-Control", "no-store")
		if !testMediaAuthorized(e.Request, token) {
			return e.JSON(401, map[string]string{"error": "Enter your test token to view uploads"})
		}
		offset := 0
		if value := e.Request.URL.Query().Get("offset"); value != "" {
			var err error
			offset, err = strconv.Atoi(value)
			if err != nil || offset < 0 || offset > 1000000 {
				return e.JSON(400, map[string]string{"error": "Invalid offset"})
			}
		}
		var records []*core.Record
		err := e.App.RecordQuery("media_item").AndWhere(dbx.NewExp("substr(storage_key, 1, 6) = 'tests/' AND upload_status = 'success' AND storage_backend != '' AND storage_bucket != ''")).OrderBy("created_at DESC", "id DESC").Limit(50).Offset(int64(offset)).All(&records)
		if err != nil {
			return err
		}
		items := make([]map[string]any, 0, len(records))
		for _, record := range records {
			items = append(items, map[string]any{"id": record.Id, "name": path.Base(strings.ReplaceAll(record.GetString("original_relative_path"), "\\", "/")), "mime_type": record.GetString("mime_type"), "size_bytes": record.GetInt("file_size"), "backend": record.GetString("storage_backend"), "stream_url": testMediaPrefix + "/" + record.Id + "/stream", "thumbnail_url": testMediaPrefix + "/" + record.Id + "/thumb", "thumbs": record.GetString("thumbs")})
		}
		var nextOffset any
		if len(records) == 50 {
			nextOffset = offset + 50
		}
		return e.JSON(200, map[string]any{"items": items, "next_offset": nextOffset})
	})
	viewerHandler := func(handler func(*core.RequestEvent) error) func(*core.RequestEvent) error {
		return func(e *core.RequestEvent) error {
			if !testMediaAuthorized(e.Request, token) {
				return e.JSON(401, map[string]string{"error": "Test viewing session expired; reconnect on the view page"})
			}
			// A viewing cookie grants access only to test objects, never other library media.
			record, err := e.App.FindRecordById("media_item", e.Request.PathValue("id"))
			if err != nil || !strings.HasPrefix(record.GetString("storage_key"), "tests/") {
				return e.NotFoundError("Test media not found", nil)
			}
			e.Request.Header.Set("X-S3-Test-Token", token)
			return handler(e)
		}
	}
	se.Router.GET(testMediaPrefix+"/{id}/stream", viewerHandler(stream))
	se.Router.HEAD(testMediaPrefix+"/{id}/stream", viewerHandler(stream))
	se.Router.GET(testMediaPrefix+"/{id}/thumb", publicTestThumbnail(thumbnail))
	se.Router.HEAD(testMediaPrefix+"/{id}/thumb", publicTestThumbnail(thumbnail))
}

// Public preview route, limited to test objects; the original stream stays authorized.
func publicTestThumbnail(handler func(*core.RequestEvent) error) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		record, err := e.App.FindRecordById("media_item", e.Request.PathValue("id"))
		if err != nil || !strings.HasPrefix(record.GetString("storage_key"), "tests/") {
			return e.NotFoundError("Test media not found", nil)
		}
		return handler(e)
	}
}
