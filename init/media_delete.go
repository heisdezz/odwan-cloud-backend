package appinit

import (
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"
)

func initializeDeletionQueue(app core.App) error {
	_, err := app.DB().NewQuery(`CREATE TABLE IF NOT EXISTS _media_storage_deletions (
 backend TEXT NOT NULL, bucket TEXT NOT NULL, key TEXT NOT NULL, etag TEXT NOT NULL DEFAULT '',
 attempts INTEGER NOT NULL DEFAULT 0, next_attempt INTEGER NOT NULL DEFAULT 0, last_error TEXT NOT NULL DEFAULT '',
 PRIMARY KEY (backend,bucket,key))`).Execute()
	return err
}

// Queue inside the deletion transaction: rolled-back deletes cannot remove remote files.
func MediaDeletionHooks(app core.App) {
	app.OnRecordDelete("media_item").Bind(&hook.Handler[*core.RecordEvent]{Id: "queue_telegram_delete", Func: func(e *core.RecordEvent) error {
		previous, err := e.App.FindRecordById("media_item", e.Record.Id)
		if err != nil {
			return err
		}
		if err := e.Next(); err != nil {
			return err
		}
		if (previous.GetString("storage_backend") != "telegram" && previous.GetString("storage_backend") != "s3") || previous.GetString("storage_bucket") == "" || previous.GetString("storage_key") == "" {
			return nil
		}
		_, err = e.App.DB().NewQuery(`INSERT INTO _media_storage_deletions (backend,bucket,key,etag) VALUES ({:backend},{:bucket},{:key},{:etag})
  ON CONFLICT(backend,bucket,key) DO UPDATE SET etag=excluded.etag, attempts=0, next_attempt=0, last_error=''`).Bind(dbx.Params{"backend": previous.GetString("storage_backend"), "bucket": previous.GetString("storage_bucket"), "key": previous.GetString("storage_key"), "etag": previous.GetString("storage_etag")}).Execute()
		return err
	}})
}
