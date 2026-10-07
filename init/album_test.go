package appinit

import (
	"errors"
	"testing"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
)

func albumTestApp(t *testing.T) *pocketbase.PocketBase {
	t.Helper()
	app := pocketbase.NewWithConfig(pocketbase.Config{DefaultDataDir: t.TempDir()})
	app.OnBootstrap().BindFunc(Initialize)
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { app.ResetBootstrapState() })
	return app
}
func createAlbum(t *testing.T, app core.App, name string) *core.Record {
	t.Helper()
	col, err := app.FindCollectionByNameOrId("album")
	if err != nil {
		t.Fatal(err)
	}
	record := core.NewRecord(col)
	record.Set("name", name)
	record.Set("relative_path", name)
	if err := app.Save(record); err != nil {
		t.Fatal(err)
	}
	return record
}
func mediaRecord(t *testing.T, app core.App, album string) *core.Record {
	t.Helper()
	col, err := app.FindCollectionByNameOrId("media_item")
	if err != nil {
		t.Fatal(err)
	}
	record := core.NewRecord(col)
	record.Set("file_hash", "test-hash")
	record.Set("original_relative_path", "test.jpg")
	record.Set("current_relative_path", "test.jpg")
	record.Set("mime_type", "image/jpeg")
	record.Set("upload_status", "pending")
	record.Set("album_id", album)
	return record
}
func assertAlbumCount(t *testing.T, app core.App, id string, count int) {
	t.Helper()
	album, err := app.FindRecordById("album", id)
	if err != nil {
		t.Fatal(err)
	}
	if got := album.GetInt("media_count"); got != count {
		t.Fatalf("album %s count: got %d, want %d", id, got, count)
	}
}
func TestAlbumCountsCreateMoveAndDelete(t *testing.T) {
	app := albumTestApp(t)
	first := createAlbum(t, app, "First")
	second := createAlbum(t, app, "Second")
	record := mediaRecord(t, app, "")
	if err := app.Save(record); err != nil {
		t.Fatal(err)
	}
	if record.GetString("album_id") != UnsortedAlbumID {
		t.Fatal("missing album did not default to unsorted")
	}
	assertAlbumCount(t, app, UnsortedAlbumID, 1)
	record.Set("album_id", first.Id)
	if err := app.Save(record); err != nil {
		t.Fatal(err)
	}
	assertAlbumCount(t, app, UnsortedAlbumID, 0)
	assertAlbumCount(t, app, first.Id, 1)
	record.Set("album_id", second.Id)
	if err := app.Save(record); err != nil {
		t.Fatal(err)
	}
	assertAlbumCount(t, app, first.Id, 0)
	assertAlbumCount(t, app, second.Id, 1)
	record.Set("metadata_json", "updated")
	if err := app.Save(record); err != nil {
		t.Fatal(err)
	}
	assertAlbumCount(t, app, second.Id, 1)
	record.Set("album_id", "")
	if err := app.Save(record); err != nil {
		t.Fatal(err)
	}
	assertAlbumCount(t, app, second.Id, 0)
	assertAlbumCount(t, app, UnsortedAlbumID, 1)
	if err := app.Delete(record); err != nil {
		t.Fatal(err)
	}
	assertAlbumCount(t, app, UnsortedAlbumID, 0)
}
func TestAlbumCountsRollbackAndFailedSave(t *testing.T) {
	app := albumTestApp(t)
	first := createAlbum(t, app, "First")
	second := createAlbum(t, app, "Second")
	record := mediaRecord(t, app, first.Id)
	if err := app.Save(record); err != nil {
		t.Fatal(err)
	}
	record.Set("album_id", second.Id)
	record.Set("mime_type", "")
	if err := app.Save(record); err == nil {
		t.Fatal("invalid media saved")
	}
	assertAlbumCount(t, app, first.Id, 1)
	assertAlbumCount(t, app, second.Id, 0)
	sentinel := errors.New("rollback")
	err := app.RunInTransaction(func(tx core.App) error {
		record := mediaRecord(t, tx, second.Id)
		if err := tx.Save(record); err != nil {
			return err
		}
		assertAlbumCount(t, tx, second.Id, 1)
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	assertAlbumCount(t, app, second.Id, 0)
}
func TestAlbumNamesAndUnsortedProtection(t *testing.T) {
	app := albumTestApp(t)
	first := createAlbum(t, app, "  Vacation  ")
	if first.GetString("name") != "Vacation" {
		t.Fatal("name not trimmed")
	}
	col, _ := app.FindCollectionByNameOrId("album")
	duplicate := core.NewRecord(col)
	duplicate.Set("name", "vacation")
	duplicate.Set("relative_path", "other")
	if err := app.Save(duplicate); err == nil {
		t.Fatal("duplicate album name accepted")
	}
	other := createAlbum(t, app, "Other")
	other.Set("name", "VACATION")
	if err := app.Save(other); err == nil {
		t.Fatal("duplicate name accepted on update")
	}
	unsorted, err := app.FindRecordById("album", UnsortedAlbumID)
	if err != nil {
		t.Fatal(err)
	}
	unsorted.Set("name", "Renamed")
	if err := app.Save(unsorted); err == nil {
		t.Fatal("default album renamed")
	}
	if err := app.Delete(unsorted); err == nil {
		t.Fatal("default album deleted")
	}
	first.Set("media_count", 999)
	if err := app.Save(first); err != nil {
		t.Fatal(err)
	}
	assertAlbumCount(t, app, first.Id, 0)
}
func TestAlbumDeleteMovesMediaToUnsorted(t *testing.T) {
	app := albumTestApp(t)
	album := createAlbum(t, app, "Temporary")
	media := mediaRecord(t, app, album.Id)
	if err := app.Save(media); err != nil {
		t.Fatal(err)
	}
	if err := app.Delete(album); err != nil {
		t.Fatal(err)
	}
	media, err := app.FindRecordById("media_item", media.Id)
	if err != nil {
		t.Fatal(err)
	}
	if media.GetString("album_id") != UnsortedAlbumID {
		t.Fatal("orphaned media not moved to unsorted")
	}
	assertAlbumCount(t, app, UnsortedAlbumID, 1)
}
func TestAlbumStartupReconciliation(t *testing.T) {
	app := albumTestApp(t)
	album := createAlbum(t, app, "Existing")
	media := mediaRecord(t, app, album.Id)
	if err := app.Save(media); err != nil {
		t.Fatal(err)
	}
	if _, err := app.NonconcurrentDB().NewQuery(`UPDATE media_item SET album_id = ''; UPDATE album SET media_count = 999`).Execute(); err != nil {
		t.Fatal(err)
	}
	if err := app.ResetBootstrapState(); err != nil {
		t.Fatal(err)
	}
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	assertAlbumCount(t, app, album.Id, 0)
	assertAlbumCount(t, app, UnsortedAlbumID, 1)
	media = mediaRecord(t, app, "")
	if err := app.Save(media); err != nil {
		t.Fatal(err)
	}
	assertAlbumCount(t, app, UnsortedAlbumID, 2)
}
