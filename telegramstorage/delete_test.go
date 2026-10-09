package telegramstorage

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/aahl/tgnas/metadata"
	"go-test/s3"
)

func TestDeleteSplitObjectRetriesAndRemovesDuplicateMapping(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	fake := &fakeTelegram{files: map[string][]byte{}}
	u, err := newUploader(s3.Config{StoragePath: dir, Telegram: s3.TelegramConfig{BotToken: "123:test", ChatID: "456", ChunkSize: 8}}, fake)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	filename := filepath.Join(dir, "file")
	os.WriteFile(filename, []byte("abcdefghijklmnopqrst"), 0600)
	result, err := u.UploadFileUnique(ctx, filename, "tests/file", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	deleted := map[int64]bool{}
	fail := true
	u.deleteMessage = func(_ context.Context, chat string, id int64) error {
		if chat != "456" {
			t.Fatal("wrong chat")
		}
		if id == 2 && fail {
			return errors.New("network failure")
		}
		deleted[id] = true
		return nil
	}
	if err := u.DeleteObject(ctx, result.Bucket, result.Key, result.ETag); err == nil {
		t.Fatal("failure was ignored")
	}
	if _, _, err := u.meta.GetObject(ctx, result.Bucket, result.Key); err != nil {
		t.Fatal("failure lost chunk references")
	}
	fail = false
	if err := u.DeleteObject(ctx, result.Bucket, result.Key, result.ETag); err != nil {
		t.Fatal(err)
	}
	if len(deleted) != 3 {
		t.Fatal("split messages left behind", deleted)
	}
	if _, _, err := u.meta.GetObject(ctx, result.Bucket, result.Key); !errors.Is(err, metadata.ErrNotFound) {
		t.Fatal("metadata not removed", err)
	}
	if err := u.DeleteObject(ctx, result.Bucket, result.Key, result.ETag); err != nil {
		t.Fatal("repeat delete failed", err)
	}
	again, err := u.UploadFileUnique(ctx, filename, result.Key, "", nil)
	if err != nil || again.Duplicate {
		t.Fatal("deleted file incorrectly deduplicated", err)
	}
}

func TestTelegramDeleteHTTPAcknowledgement(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		body    string
		success bool
	}{
		{"deleted", 200, `{"ok":true,"result":true}`, true},
		{"already missing", 400, `{"ok":false,"description":"Bad Request: message to delete not found"}`, true},
		{"too old", 400, `{"ok":false,"description":"Bad Request: message can't be deleted"}`, false},
		{"forbidden", 403, `{"ok":false,"description":"Forbidden"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "POST" || r.URL.Path != "/bot123:test/deleteMessage" {
					t.Errorf("wrong delete request")
				}
				if err := r.ParseForm(); err != nil || r.Form.Get("chat_id") != "456" || r.Form.Get("message_id") != "7" {
					t.Errorf("wrong delete parameters")
				}
				w.WriteHeader(tc.status)
				fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			err := telegramMessageDeleter("123:test", server.URL)(context.Background(), "456", 7)
			if (err == nil) != tc.success {
				t.Fatal("incorrect delete acknowledgement", err)
			}
		})
	}
}
