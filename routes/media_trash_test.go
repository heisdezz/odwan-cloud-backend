package routes

import (
	"context"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
	"go-test/s3"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTrashRestoreExpiryAndPermissions(t *testing.T) {
	app := mediaTestApp(t)
	record, err := saveUploadedMedia(app, s3.Result{Backend: "telegram", Bucket: "telegram", Key: "tests/trash", Hash: strings.Repeat("b", 64), ETag: "etag"}, "file.mp4", "video/mp4", 10)
	if err != nil {
		t.Fatal(err)
	}
	col, err := app.FindCollectionByNameOrId("_superusers")
	if err != nil {
		t.Fatal(err)
	}
	admin := core.NewRecord(col)
	admin.SetEmail("test@example.com")
	admin.SetPassword("test-password-1234")
	if err := app.Save(admin); err != nil {
		t.Fatal(err)
	}
	call := func(action string, auth *core.Record) error {
		req := httptest.NewRequest("POST", "/api/media/"+record.Id+"/"+action, nil)
		req.SetPathValue("id", record.Id)
		return mediaTrashHandler(action)(&core.RequestEvent{App: app, Auth: auth, Event: router.Event{Request: req, Response: httptest.NewRecorder()}})
	}
	if err := call("trash", nil); err == nil {
		t.Fatal("anonymous trash accepted")
	}
	if err := call("permanent", admin); err == nil {
		t.Fatal("active media deleted permanently")
	}
	if err := call("trash", admin); err != nil {
		t.Fatal(err)
	}
	fresh, _ := app.FindRecordById("media_item", record.Id)
	expires := fresh.GetInt("trash_expires_at")
	if fresh.GetInt("trashed_at") == 0 || int64(expires) <= time.Now().UnixMilli() {
		t.Fatal("trash expiry not saved")
	}
	var count int
	if err := app.DB().NewQuery(`SELECT COUNT(*) FROM _media_storage_deletions`).Row(&count); err != nil || count != 0 {
		t.Fatal("soft trash queued cloud deletion", err)
	}
	if err := call("trash", admin); err != nil {
		t.Fatal(err)
	}
	fresh, _ = app.FindRecordById("media_item", record.Id)
	if fresh.GetInt("trash_expires_at") != expires {
		t.Fatal("repeated trash extended recovery period")
	}
	if err := call("restore", admin); err != nil {
		t.Fatal(err)
	}
	fresh, _ = app.FindRecordById("media_item", record.Id)
	if fresh.GetInt("trashed_at") != 0 {
		t.Fatal("restore failed")
	}
	if err := call("trash", admin); err != nil {
		t.Fatal(err)
	}
	fresh, _ = app.FindRecordById("media_item", record.Id)
	fresh.Set("trash_expires_at", time.Now().Add(-time.Minute).UnixMilli())
	if err := app.Save(fresh); err != nil {
		t.Fatal(err)
	}
	if err := call("restore", admin); err == nil {
		t.Fatal("expired media restored")
	}
	purgeExpiredMedia(context.Background(), app)
	if _, err := app.FindRecordById("media_item", record.Id); err == nil {
		t.Fatal("expired media retained")
	}
	if err := app.DB().NewQuery(`SELECT COUNT(*) FROM _media_storage_deletions`).Row(&count); err != nil || count != 1 {
		t.Fatal("expired trash cleanup not queued", err, count)
	}
}
