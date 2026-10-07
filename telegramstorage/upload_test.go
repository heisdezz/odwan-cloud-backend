package telegramstorage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/aahl/tgnas/telegram"
	"go-test/s3"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeTelegram struct {
	calls     int
	fail      bool
	files     map[string][]byte
	requests  []telegram.UploadRequest
	downloads []string
}

func (f *fakeTelegram) Upload(ctx context.Context, r telegram.UploadRequest) (telegram.UploadedFile, error) {
	f.calls++
	f.requests = append(f.requests, r)
	if f.fail && f.calls == 2 {
		return telegram.UploadedFile{}, errors.New("interrupted")
	}
	b, err := io.ReadAll(r.Reader)
	if err != nil {
		return telegram.UploadedFile{}, err
	}
	id := fmt.Sprint(f.calls)
	f.files[id] = b
	return telegram.UploadedFile{Type: telegram.TypeDocument, FileID: id, FileSize: int64(len(b)), MessageID: int64(f.calls)}, nil
}
func (f *fakeTelegram) Download(ctx context.Context, id string) (io.ReadCloser, error) {
	f.downloads = append(f.downloads, id)
	return io.NopCloser(bytes.NewReader(f.files[id])), nil
}
func TestResumeRestartDownloadAndDeduplicate(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	c := s3.Config{StoragePath: dir, Telegram: s3.TelegramConfig{BotToken: "123:test", ChatID: "456", ChunkSize: 8}}
	f := &fakeTelegram{fail: true, files: map[string][]byte{}}
	u, err := newUploader(c, f)
	if err != nil {
		t.Fatal(err)
	}
	data := []byte("abcdefghijklmnopqrst")
	filename := filepath.Join(dir, "source")
	if err := os.WriteFile(filename, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = u.UploadFileUnique(ctx, filename, "tests/file", "", nil); err == nil {
		t.Fatal("expected interrupted upload")
	}
	if err = u.Close(); err != nil {
		t.Fatal(err)
	}
	u, err = newUploader(c, f)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	result, err := u.UploadFileUnique(ctx, filename, "tests/file", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if f.calls != 4 || result.Duplicate || result.Backend != "telegram" {
		t.Fatalf("calls=%d result=%+v", f.calls, result)
	}
	reader, _, err := u.GetObject(ctx, result.Key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(reader)
	reader.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("download=%q error=%v", got, err)
	}
	duplicate, err := u.UploadFileUnique(ctx, filename, "tests/other", result.Hash, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !duplicate.Duplicate || duplicate.Key != result.Key || f.calls != 4 {
		t.Fatalf("duplicate=%+v calls=%d", duplicate, f.calls)
	}
	_, err = u.UploadFileUnique(ctx, filename, "tests/other", strings.Repeat("0", 64), nil)
	if !errors.Is(err, s3.ErrHashMismatch) {
		t.Fatalf("hash mismatch: %v", err)
	}
	if f.calls != 4 {
		t.Fatal("mismatched hash uploaded")
	}
}

func TestTelegramDocumentNames(t *testing.T) {
	for _, test := range []struct {
		name      string
		chunkSize int64
		want      []string
	}{
		{"single", 100, []string{"photo.jpg"}},
		{"chunks", 4, []string{"photo.jpg.part000001", "photo.jpg.part000002"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			dir := t.TempDir()
			c := s3.Config{StoragePath: dir, Telegram: s3.TelegramConfig{BotToken: "123:test", ChatID: "456", ChunkSize: test.chunkSize}}
			f := &fakeTelegram{files: map[string][]byte{}}
			u, err := newUploader(c, f)
			if err != nil {
				t.Fatal(err)
			}
			defer u.Close()
			filename := filepath.Join(dir, "staged-no-extension")
			if err := os.WriteFile(filename, []byte{0xff, 0xd8, 0xff, 0xe0, 0, 0, 0, 0}, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := u.UploadFileUnique(context.Background(), filename, "tests/photo.jpg", "", nil); err != nil {
				t.Fatal(err)
			}
			if len(f.requests) != len(test.want) {
				t.Fatalf("requests=%d", len(f.requests))
			}
			for i, want := range test.want {
				if f.requests[i].Filename != want {
					t.Fatalf("filename=%q want=%q", f.requests[i].Filename, want)
				}
				wantType := "image/jpeg"
				if len(test.want) > 1 {
					wantType = "application/octet-stream"
				}
				if f.requests[i].MIMEType != wantType {
					t.Fatalf("MIME=%q want=%q", f.requests[i].MIMEType, wantType)
				}
			}
		})
	}
}

func TestRangeFetchesOnlyRelevantTelegramChunks(t *testing.T) {
	dir := t.TempDir()
	c := s3.Config{StoragePath: dir, Telegram: s3.TelegramConfig{BotToken: "123:test", ChatID: "456", ChunkSize: 4}}
	f := &fakeTelegram{files: map[string][]byte{}}
	u, err := newUploader(c, f)
	if err != nil {
		t.Fatal(err)
	}
	defer u.Close()
	filename := filepath.Join(dir, "file")
	if err := os.WriteFile(filename, []byte("0123456789ab"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := u.UploadFileUnique(context.Background(), filename, "tests/video.mp4", "", nil); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		start, end int64
		want       string
		downloads  int
	}{
		{5, 6, "56", 1}, {3, 8, "345678", 3},
	} {
		f.downloads = nil
		reader, err := u.OpenObject(context.Background(), "telegram", "tests/video.mp4", &s3.ByteRange{Start: test.start, End: test.end})
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(reader)
		reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != test.want || len(f.downloads) != test.downloads {
			t.Fatalf("got=%q downloads=%v", got, f.downloads)
		}
	}
}
