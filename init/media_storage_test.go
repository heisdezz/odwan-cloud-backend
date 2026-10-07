package appinit

import (
	"github.com/pocketbase/pocketbase"
	"testing"
)

func TestMediaStorageMigrationPreservesRecordsAndRules(t *testing.T) {
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
	public := ""
	collection.ViewRule = &public
	for _, name := range []string{"storage_backend", "storage_bucket", "storage_key", "storage_etag"} {
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
	for _, name := range []string{"storage_backend", "storage_bucket", "storage_key", "storage_etag"} {
		if collection.Fields.GetByName(name) == nil {
			t.Fatalf("missing field %s", name)
		}
	}
	if collection.ViewRule == nil || *collection.ViewRule != "" {
		t.Fatal("existing view rule changed")
	}
	if _, err := app.FindRecordById("media_item", record.Id); err != nil {
		t.Fatal("existing media lost", err)
	}
}
