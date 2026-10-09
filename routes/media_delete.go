package routes

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"
)

type objectDeleter interface {
	DeleteObject(context.Context, string, string, string) error
}
type deletionJob struct {
	Backend  string `db:"backend"`
	Bucket   string `db:"bucket"`
	Key      string `db:"key"`
	ETag     string `db:"etag"`
	Attempts int    `db:"attempts"`
}

func registerMediaCleanup(se *core.ServeEvent, registry *storageRegistry) (func(), <-chan struct{}) {
	ctx, cancel := context.WithCancel(context.Background())
	wake := make(chan struct{}, 1)
	done := make(chan struct{})
	se.App.OnRecordAfterDeleteSuccess("media_item").Bind(&hook.Handler[*core.RecordEvent]{Id: "wake_media_cleanup", Func: func(e *core.RecordEvent) error {
		if err := e.Next(); err != nil {
			return err
		}
		select {
		case wake <- struct{}{}:
		default:
		}
		return nil
	}})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			processMediaDeletions(ctx, se.App, registry, &registry.lifecycle)
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-wake:
			}
		}
	}()
	return cancel, done
}

func processMediaDeletions(ctx context.Context, app core.App, registry *storageRegistry, lifecycle *sync.Mutex) {
	purgeExpiredMedia(ctx, app)
	var jobs []deletionJob
	if err := app.DB().NewQuery(`SELECT backend,bucket,key,etag,attempts FROM _media_storage_deletions WHERE next_attempt <= {:now} ORDER BY next_attempt LIMIT 10`).Bind(dbx.Params{"now": time.Now().Unix()}).All(&jobs); err != nil {
		app.Logger().Error("read media deletion queue", "error", err)
		return
	}
	for _, job := range jobs {
		if ctx.Err() != nil {
			return
		}
		lifecycle.Lock()
		processDeletionJob(ctx, app, registry, job)
		lifecycle.Unlock()
	}
}
func processDeletionJob(ctx context.Context, app core.App, registry *storageRegistry, job deletionJob) {
	params := dbx.Params{"backend": job.Backend, "bucket": job.Bucket, "key": job.Key}
	var references int
	err := app.DB().NewQuery(`SELECT COUNT(*) FROM media_item WHERE storage_backend={:backend} AND storage_bucket={:bucket} AND storage_key={:key}`).Bind(params).Row(&references)
	if err == nil && references == 0 {
		client, resolveErr := registry.get(job.Backend)
		err = resolveErr
		if err == nil {
			deleter, ok := client.(objectDeleter)
			if !ok {
				err = fmt.Errorf("backend cannot delete objects")
			} else {
				jobCtx, cancel := context.WithTimeout(ctx, time.Minute)
				err = deleter.DeleteObject(jobCtx, job.Bucket, job.Key, job.ETag)
				cancel()
			}
		}
	}
	if err == nil {
		if _, err = app.DB().NewQuery(`DELETE FROM _media_storage_deletions WHERE backend={:backend} AND bucket={:bucket} AND key={:key}`).Bind(params).Execute(); err != nil {
			app.Logger().Error("remove media deletion job", "error", err)
		}
		return
	}
	if ctx.Err() != nil {
		return
	}
	delay := min(6*time.Hour, time.Minute*time.Duration(1<<min(job.Attempts, 9)))
	params["attempts"] = job.Attempts + 1
	params["next"] = time.Now().Add(delay).Unix()
	params["error"] = err.Error()
	if _, saveErr := app.DB().NewQuery(`UPDATE _media_storage_deletions SET attempts={:attempts},next_attempt={:next},last_error={:error} WHERE backend={:backend} AND bucket={:bucket} AND key={:key}`).Bind(params).Execute(); saveErr != nil {
		app.Logger().Error("save media deletion retry", "error", saveErr)
	}
	app.Logger().Error("Media cleanup failed; retry scheduled", "bucket", job.Bucket, "key", job.Key, "error", err)
}
