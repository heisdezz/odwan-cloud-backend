package appinit

import (
	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
	"testing"
)

func TestMediaStorageMigrationEnablesPublicReadsAndPreservesWrites(t *testing.T) {
	dir := t.TempDir()
	app := pocketbase.NewWithConfig(pocketbase.Config{DefaultDataDir: dir})
	app.OnBootstrap().BindFunc(Initialize)
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	record := mediaRecord(t, app, "unsorted")
	if err := app.Save(record); err != nil {
		t.Fatal(err)
	}
	collection, err := app.FindCollectionByNameOrId("media_item")
	if err != nil {
		t.Fatal(err)
	}
	collection.ViewRule = nil
	collection.ListRule = nil
	for _, name := range []string{"storage_backend", "storage_bucket", "storage_key", "storage_etag", "thumbs"} {
		collection.Fields.RemoveByName(name)
	}
	collection.RemoveIndex("idx_media_storage_object")
	if err := app.Save(collection); err != nil {
		t.Fatal(err)
	}
	if err := app.ResetBootstrapState(); err != nil {
		t.Fatal(err)
	}
	if err := app.Bootstrap(); err != nil {
		t.Fatal(err)
	}
	defer app.ResetBootstrapState()
	collection, err = app.FindCollectionByNameOrId("media_item")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"storage_backend", "storage_bucket", "storage_key", "storage_etag", "thumbs"} {
		if collection.Fields.GetByName(name) == nil {
			t.Fatalf("missing field %s", name)
		}
	}
	thumbs, ok := collection.Fields.GetByName("thumbs").(*core.FileField)
	if !ok || thumbs.Protected || thumbs.MaxSelect != 1 {
		t.Fatal("thumbnail field must store one public file")
	}
	if collection.ViewRule == nil || *collection.ViewRule != "" {
		t.Fatal("media reads are not public")
	}
	if collection.ListRule == nil || *collection.ListRule != "" {
		t.Fatal("media list is not public")
	}
	if collection.CreateRule != nil || collection.UpdateRule != nil || collection.DeleteRule != nil {
		t.Fatal("write rules changed")
	}
	if _, err := app.FindRecordById("media_item", record.Id); err != nil {
		t.Fatal("existing media lost", err)
	}
}
