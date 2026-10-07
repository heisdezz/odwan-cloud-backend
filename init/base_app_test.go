package appinit

import (
	"testing"

	"github.com/pocketbase/pocketbase"
	"github.com/pocketbase/pocketbase/core"
)

func TestInitialize(t *testing.T) {
	dataDir := t.TempDir()
	var tagID string
	for run := 0; run < 2; run++ {
		app := pocketbase.NewWithConfig(pocketbase.Config{DefaultDataDir: dataDir})
		app.OnBootstrap().BindFunc(Initialize)
		if err := app.Bootstrap(); err != nil {
			app.ResetBootstrapState()
			t.Fatal(err)
		}
		func() {
			defer app.ResetBootstrapState()
			for _, name := range []string{"media_item", "album", "tag", "media_tag", "library_stats"} {
				col, err := app.FindCollectionByNameOrId(name)
				if err != nil {
					t.Fatal(err)
				}
				if len(col.Fields) <= 1 {
					t.Fatalf("%s has no custom fields", name)
				}
			}
			if run == 0 {
				col, _ := app.FindCollectionByNameOrId("tag")
				record := core.NewRecord(col)
				record.Set("name", "Nature")
				record.Set("color_hex", "#00ff00")
				record.Set("category", "subject")
				if err := app.Save(record); err != nil {
					t.Fatal(err)
				}
				tagID = record.Id
			} else if _, err := app.FindRecordById("tag", tagID); err != nil {
				t.Fatalf("existing record lost after restart: %v", err)
			}
		}()
	}
}
