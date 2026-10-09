package appinit

import (
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"
)

// A thumbnail belongs to the source, not to mutable album metadata.
func thumbnailHooks(app core.App) {
	app.OnRecordUpdate("media_item").Bind(&hook.Handler[*core.RecordEvent]{Id: "invalidate_media_thumbnail", Func: func(e *core.RecordEvent) error {
		previous, err := e.App.FindRecordById("media_item", e.Record.Id)
		if err != nil {
			return err
		}
		for _, field := range []string{"file_hash", "storage_backend", "storage_bucket", "storage_key", "storage_etag", "mime_type", "upload_status"} {
			if previous.GetString(field) != e.Record.GetString(field) {
				e.Record.Set("thumbs", "")
				break
			}
		}
		return e.Next()
	}})
}
