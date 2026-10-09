package routes

import (
	"crypto/rand"
	_ "embed"
	"encoding/base64"
	"fmt"
	"path"
	"strconv"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

//go:embed test_media.html
var testMediaPage string

const testMediaPrefix = "/api/test/media"

func registerTestMediaRoutes(se *core.ServeEvent, _ string, stream, thumbnail func(*core.RequestEvent) error) {
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
	se.Router.GET(testMediaPrefix, func(e *core.RequestEvent) error {
		e.Response.Header().Set("Cache-Control", "no-store")
		offset := 0
		if value := e.Request.URL.Query().Get("offset"); value != "" {
			var err error
			offset, err = strconv.Atoi(value)
			if err != nil || offset < 0 || offset > 1000000 {
				return e.JSON(400, map[string]string{"error": "Invalid offset"})
			}
		}
		var records []*core.Record
		err := e.App.RecordQuery("media_item").AndWhere(dbx.NewExp("substr(storage_key, 1, 6) = 'tests/' AND trashed_at = 0 AND upload_status = 'success' AND storage_backend != '' AND storage_bucket != ''")).OrderBy("created_at DESC", "id DESC").Limit(50).Offset(int64(offset)).All(&records)
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
	se.Router.GET(testMediaPrefix+"/{id}/stream", publicTestMedia(stream))
	se.Router.HEAD(testMediaPrefix+"/{id}/stream", publicTestMedia(stream))
	se.Router.GET(testMediaPrefix+"/{id}/thumb", publicTestMedia(thumbnail))
	se.Router.HEAD(testMediaPrefix+"/{id}/thumb", publicTestMedia(thumbnail))
}

// Test viewing routes expose only objects under tests/.
func publicTestMedia(handler func(*core.RequestEvent) error) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		record, err := e.App.FindRecordById("media_item", e.Request.PathValue("id"))
		if err != nil || !strings.HasPrefix(record.GetString("storage_key"), "tests/") {
			return e.NotFoundError("Test media not found", nil)
		}
		return handler(e)
	}
}
