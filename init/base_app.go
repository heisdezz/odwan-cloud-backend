package appinit

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/pocketbase/pocketbase/core"
)

// Initialize creates missing collections after the bootstrap chain completes.
func Initialize(be *core.BootstrapEvent) error {
	if err := be.Next(); err != nil {
		return err
	}
	Album_Count(be.App)
	thumbnailHooks(be.App)
	MediaDeletionHooks(be.App)
	return be.App.RunInTransaction(func(app core.App) error {
		if err := initializeDeletionQueue(app); err != nil {
			return err
		}
		names := []string{"media_item", "album", "tag", "media_tag", "library_stats"}
		collections := map[string]*core.Collection{}
		missing := map[string]bool{}
		// Create targets first to support the circular album/media relations.
		for _, name := range names {
			col, err := app.FindCollectionByNameOrId(name)
			if errors.Is(err, sql.ErrNoRows) {
				col = core.NewBaseCollection(name)
				if err := app.Save(col); err != nil {
					return fmt.Errorf("create %s: %w", name, err)
				}
				missing[name] = true
			} else if err != nil {
				return fmt.Errorf("find %s: %w", name, err)
			}
			collections[name] = col
		}
		text := func(name string, required bool) core.Field { return &core.TextField{Name: name, Required: required} }
		number := func(name string) core.Field { return &core.NumberField{Name: name, OnlyInt: true} }
		created := func() core.Field { return &core.AutodateField{Name: "created_at", OnCreate: true} }
		relation := func(name, target string, required, cascade bool) core.Field {
			return &core.RelationField{Name: name, CollectionId: collections[target].Id, MaxSelect: 1, Required: required, CascadeDelete: cascade}
		}
		// Expanded album_name and tags are derived through relations, not stored fields.
		fields := map[string][]core.Field{
			"media_item": {
				text("file_hash", true), text("original_relative_path", true), text("current_relative_path", true),
				number("file_size"), text("mime_type", true), &core.NumberField{Name: "duration_seconds"},
				text("metadata_json", false), relation("album_id", "album", false, false), created(),
				&core.SelectField{Name: "upload_status", Values: []string{"pending", "uploading", "success", "error"}, MaxSelect: 1, Required: true},
			},
			"album": {
				text("name", true), text("relative_path", true), text("description", false), number("media_count"), created(),
				relation("cover_media_id", "media_item", false, false),
			},
			"tag": {
				text("name", true), &core.TextField{Name: "color_hex", Required: true, Pattern: `^#[0-9a-fA-F]{6}$`},
				text("category", true), number("media_count"),
			},
			"media_tag": {relation("media_id", "media_item", true, true), relation("tag_id", "tag", true, true)},
			"library_stats": {
				number("total_items"), number("images"), number("videos"), number("albums"), number("tags"), number("db_size_bytes"),
				text("db_size_formatted", false), &core.BoolField{Name: "db_exists"},
			},
		}
		for _, name := range names {
			if !missing[name] {
				continue
			}
			col := collections[name]
			col.Fields.Add(fields[name]...)
			if name == "media_tag" {
				col.AddIndex("idx_media_tag_pair", true, "media_id, tag_id", "")
			}
			if err := app.Save(col); err != nil {
				return fmt.Errorf("configure %s: %w", name, err)
			}
		}
		// Add fields to existing installations while preserving write permissions.
		media := collections["media_item"]
		for _, field := range []core.Field{
			&core.SelectField{Name: "storage_backend", Values: []string{"telegram", "s3"}, MaxSelect: 1},
			text("storage_bucket", false), text("storage_key", false), text("storage_etag", false),
			number("trashed_at"), number("trash_expires_at"),
			&core.FileField{Name: "thumbs", MaxSelect: 1, MaxSize: 1 << 20, MimeTypes: []string{"image/jpeg"}, Protected: false},
		} {
			if media.Fields.GetByName(field.GetName()) == nil {
				media.Fields.Add(field)
			}
		}
		if thumbs, ok := media.Fields.GetByName("thumbs").(*core.FileField); ok {
			thumbs.Protected = false
		}
		public := ""
		media.ListRule, media.ViewRule = &public, &public
		media.AddIndex("idx_media_storage_object", true, "storage_backend, storage_bucket, storage_key", "storage_key != ''")
		if err := app.Save(media); err != nil {
			return fmt.Errorf("configure media storage: %w", err)
		}
		return initializeAlbums(app)
	})
}
