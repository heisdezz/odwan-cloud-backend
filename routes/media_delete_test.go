package routes

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"go-test/s3"
)

type deletionStorage struct {
	streamStub
	calls int
	fail  bool
}

func (s *deletionStorage) UploadFileUnique(context.Context, string, string, string, func(s3.Progress)) (s3.Result, error) {
	return s3.Result{}, nil
}
func (s *deletionStorage) DeleteObject(context.Context, string, string, string) error {
	s.calls++
	if s.fail {
		return errors.New("remote failure")
	}
	return nil
}
func TestMediaDeletionQueueRollbackRetryAndCompletion(t *testing.T) {
	app := mediaTestApp(t)
	record, err := saveUploadedMedia(app, s3.Result{Backend: "telegram", Bucket: "telegram", Key: "tests/delete", Hash: strings.Repeat("a", 64), ETag: "etag"}, "file.mp4", "video/mp4", 10)
	if err != nil {
		t.Fatal(err)
	}
	err = app.RunInTransaction(func(tx core.App) error {
		if err := tx.Delete(record); err != nil {
			return err
		}
		return errors.New("rollback")
	})
	if err == nil {
		t.Fatal("expected rollback")
	}
	var count int
	if err := app.DB().NewQuery(`SELECT COUNT(*) FROM _media_storage_deletions`).Row(&count); err != nil || count != 0 {
		t.Fatal("rolled back delete queued", count, err)
	}
	fresh, err := app.FindRecordById("media_item", record.Id)
	if err != nil {
		t.Fatal(err)
	}
	if err := app.Delete(fresh); err != nil {
		t.Fatal(err)
	}
	storage := &deletionStorage{fail: true}
	registry := &storageRegistry{clients: map[string]fileUploader{"telegram": storage}}
	processMediaDeletions(context.Background(), app, registry, &registry.lifecycle)
	var attempts int
	if err := app.DB().NewQuery(`SELECT attempts FROM _media_storage_deletions`).Row(&attempts); err != nil || attempts != 1 || storage.calls != 1 {
		t.Fatal("failed cleanup not retained", attempts, err)
	}
	processMediaDeletions(context.Background(), app, registry, &registry.lifecycle)
	if storage.calls != 1 {
		t.Fatal("retry backoff ignored")
	}
	storage.fail = false
	app.DB().NewQuery(`UPDATE _media_storage_deletions SET next_attempt=0`).Execute()
	processMediaDeletions(context.Background(), app, registry, &registry.lifecycle)
	app.DB().NewQuery(`SELECT COUNT(*) FROM _media_storage_deletions`).Row(&count)
	if count != 0 || storage.calls != 2 {
		t.Fatal("job not completed", count, storage.calls)
	}
}
