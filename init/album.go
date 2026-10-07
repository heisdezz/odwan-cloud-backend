package appinit

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"
)

const UnsortedAlbumID = "unsorted"

// Album_Count registers transactional count maintenance. Stable handler IDs make
// registration safe to repeat when an application is bootstrapped again.
func Album_Count(app core.App) {
	saveMedia := func(e *core.RecordEvent) error {
		originalApp := e.App
		defer func() { e.App = originalApp }()
		return originalApp.RunInTransaction(func(tx core.App) error {
			e.App = tx
			oldAlbum := ""
			if e.Type == core.ModelEventTypeUpdate {
				previous, err := tx.FindRecordById("media_item", e.Record.Id)
				if err != nil {
					return err
				}
				oldAlbum = previous.GetString("album_id")
			}
			if e.Record.GetString("album_id") == "" {
				e.Record.Set("album_id", UnsortedAlbumID)
			}
			if err := e.Next(); err != nil {
				return err
			}
			return recountAlbums(tx, oldAlbum, e.Record.GetString("album_id"))
		})
	}
	app.OnRecordCreate("media_item").Bind(&hook.Handler[*core.RecordEvent]{Id: "album_count_create", Func: saveMedia})
	app.OnRecordUpdate("media_item").Bind(&hook.Handler[*core.RecordEvent]{Id: "album_count_update", Func: saveMedia})
	app.OnRecordDelete("media_item").Bind(&hook.Handler[*core.RecordEvent]{Id: "album_count_delete", Func: func(e *core.RecordEvent) error {
		originalApp := e.App
		defer func() { e.App = originalApp }()
		return originalApp.RunInTransaction(func(tx core.App) error {
			e.App = tx
			previous, err := tx.FindRecordById("media_item", e.Record.Id)
			if err != nil {
				return err
			}
			albumID := previous.GetString("album_id")
			if err := e.Next(); err != nil {
				return err
			}
			return recountAlbums(tx, albumID)
		})
	}})
	normalizeAlbum := func(e *core.RecordEvent) error {
		name := strings.TrimSpace(e.Record.GetString("name"))
		if e.Record.Id == UnsortedAlbumID && name != "unsorted" {
			return fmt.Errorf("the unsorted album cannot be renamed")
		}
		e.Record.Set("name", name)
		originalApp := e.App
		defer func() { e.App = originalApp }()
		return originalApp.RunInTransaction(func(tx core.App) error {
			e.App = tx
			var count int
			if err := tx.DB().NewQuery("SELECT COUNT(*) FROM media_item WHERE album_id = {:id}").Bind(dbx.Params{"id": e.Record.Id}).Row(&count); err != nil {
				return err
			}
			e.Record.Set("media_count", count)
			return e.Next()
		})
	}
	app.OnRecordCreate("album").Bind(&hook.Handler[*core.RecordEvent]{Id: "album_name_create", Func: normalizeAlbum})
	app.OnRecordUpdate("album").Bind(&hook.Handler[*core.RecordEvent]{Id: "album_name_update", Func: normalizeAlbum})
	app.OnRecordDelete("album").Bind(&hook.Handler[*core.RecordEvent]{Id: "protect_unsorted_album", Func: func(e *core.RecordEvent) error {
		if e.Record.Id == UnsortedAlbumID {
			return fmt.Errorf("the unsorted album cannot be deleted")
		}
		return e.Next()
	}})
}

func recountAlbums(app core.App, ids ...string) error {
	seen := map[string]bool{}
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if _, err := app.NonconcurrentDB().NewQuery(`UPDATE album SET media_count = (SELECT COUNT(*) FROM media_item WHERE album_id = {:id}) WHERE id = {:id}`).Bind(dbx.Params{"id": id}).Execute(); err != nil {
			return fmt.Errorf("recount album %s: %w", id, err)
		}
	}
	return nil
}

// initializeAlbums upgrades existing album schemas and repairs persisted counts.
func initializeAlbums(app core.App) error {
	collection, err := app.FindCollectionByNameOrId("album")
	if err != nil {
		return err
	}
	idField, ok := collection.Fields.GetByName("id").(*core.TextField)
	if !ok {
		return fmt.Errorf("album id field must be text")
	}
	idField.Min = len(UnsortedAlbumID)
	idField.Pattern = "^(unsorted|[a-z0-9]{15})$"
	collection.AddIndex("idx_album_name_unique", true, "name COLLATE NOCASE", "")
	if err := app.Save(collection); err != nil {
		return fmt.Errorf("configure unique album names: %w", err)
	}
	album, err := app.FindRecordById("album", UnsortedAlbumID)
	if err != nil {
		// Distinguish a missing record from database errors.
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		album = core.NewRecord(collection)
		album.Id = UnsortedAlbumID
		album.Set("name", "unsorted")
		album.Set("relative_path", "unsorted")
		if err := app.Save(album); err != nil {
			return fmt.Errorf("create unsorted album: %w", err)
		}
	} else if album.GetString("name") != "unsorted" {
		return fmt.Errorf("album id unsorted is reserved for the default album")
	}
	if _, err := app.NonconcurrentDB().NewQuery(`UPDATE media_item SET album_id = {:id} WHERE album_id = '' OR album_id IS NULL OR NOT EXISTS (SELECT 1 FROM album WHERE album.id = media_item.album_id)`).Bind(dbx.Params{"id": UnsortedAlbumID}).Execute(); err != nil {
		return err
	}
	_, err = app.NonconcurrentDB().NewQuery(`UPDATE album SET media_count = (SELECT COUNT(*) FROM media_item WHERE media_item.album_id = album.id)`).Execute()
	return err
}
