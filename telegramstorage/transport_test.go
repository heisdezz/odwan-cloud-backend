package telegramstorage

import (
	"bytes"
	"context"
	"fmt"
	"github.com/aahl/tgnas/telegram"
	"go-test/s3"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDocumentAcknowledgementsPreserveMediaFileIDs(t *testing.T) {
	for _, kind := range []string{"document", "video", "audio", "animation"} {
		t.Run(kind, func(t *testing.T) {
			calls := 0
			transport := documentTransport{next: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				calls++
				defer r.Body.Close()
				_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
				if err != nil {
					t.Fatal(err)
				}
				reader := multipart.NewReader(r.Body, params["boundary"])
				fields := map[string]string{}
				for {
					part, err := reader.NextPart()
					if err == io.EOF {
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					body, err := io.ReadAll(part)
					if err != nil {
						t.Fatal(err)
					}
					fields[part.FormName()] = string(body)
					if part.FormName() == "document" && part.FileName() != "video.mp4" {
						t.Fatal("filename lost")
					}
				}
				if fields["disable_content_type_detection"] != "true" || fields["document"] != "original bytes" {
					t.Fatalf("fields=%v", fields)
				}
				body := fmt.Sprintf(`{"ok":true,"result":{"message_id":7,"%s":{"file_id":"uploaded","file_unique_id":"unique","file_size":14,"mime_type":"video/mp4"}}}`, kind)
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})}
			client := telegram.NewHTTPClient("123:test", "https://example.com", &http.Client{Transport: transport})
			result, err := client.Upload(context.Background(), telegram.UploadRequest{Type: telegram.TypeDocument, ChatID: "456", Reader: bytes.NewReader([]byte("original bytes")), Filename: "video.mp4", MIMEType: "video/mp4"})
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || result.FileID != "uploaded" || result.MessageID != 7 {
				t.Fatalf("calls=%d result=%+v", calls, result)
			}
			// Exercise TgNAS and the persistent adapter, not just its response parser.
			dir := t.TempDir()
			uploader, err := newUploader(s3.Config{StoragePath: dir, Telegram: s3.TelegramConfig{BotToken: "123:test", ChatID: "456", ChunkSize: 100}}, client)
			if err != nil {
				t.Fatal(err)
			}
			defer uploader.Close()
			filename := filepath.Join(dir, "source")
			if err := os.WriteFile(filename, []byte("original bytes"), 0600); err != nil {
				t.Fatal(err)
			}
			stored, err := uploader.UploadFileUnique(context.Background(), filename, "tests/video.mp4", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Duplicate || calls != 2 {
				t.Fatalf("stored=%+v calls=%d", stored, calls)
			}
			duplicate, err := uploader.UploadFileUnique(context.Background(), filename, "tests/again.mp4", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			if !duplicate.Duplicate || calls != 2 {
				t.Fatalf("duplicate=%+v calls=%d", duplicate, calls)
			}

		})
	}
}
