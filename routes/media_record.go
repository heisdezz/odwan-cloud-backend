package routes

import (
	"database/sql"
	"errors"
	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"go-test/s3"
)

// Commit the mapping after storage succeeds; retries repair a failed DB save.
func saveUploadedMedia(app core.App, result s3.Result, filename, contentType string, size int64) (*core.Record, error) {
	var saved *core.Record
	err := app.RunInTransaction(func(tx core.App) error {
		params := dbx.Params{"backend": result.Backend, "bucket": result.Bucket, "key": result.Key}
		record, err := tx.FindFirstRecordByFilter("media_item", "storage_backend = {:backend} && storage_bucket = {:bucket} && storage_key = {:key}", params)
		if errors.Is(err, sql.ErrNoRows) {
			// Adopt an existing library record with the same hash instead of duplicating it.
			record, err = tx.FindFirstRecordByFilter("media_item", "file_hash = {:hash} && storage_key = ''", dbx.Params{"hash": result.Hash})
		}
		if errors.Is(err, sql.ErrNoRows) {
			collection, err := tx.FindCollectionByNameOrId("media_item")
			if err != nil {
				return err
			}
			record = core.NewRecord(collection)
			record.Set("original_relative_path", filename)
		} else if err != nil {
			return err
		}
		record.Set("file_hash", result.Hash)
		record.Set("file_size", size)
		record.Set("mime_type", contentType)
		record.Set("current_relative_path", result.Key)
		record.Set("storage_backend", result.Backend)
		record.Set("storage_bucket", result.Bucket)
		record.Set("storage_key", result.Key)
		record.Set("storage_etag", result.ETag)
		record.Set("upload_status", "success")
		if err := tx.Save(record); err != nil {
			return err
		}
		saved = record
		return nil
	})
	return saved, err
}
