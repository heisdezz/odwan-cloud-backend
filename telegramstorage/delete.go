package telegramstorage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aahl/tgnas/metadata"
)

// Remove remote messages before metadata, so failures keep retryable references.
func (u *Uploader) DeleteObject(ctx context.Context, bucket, key, expectedETag string) error {
	if bucket != u.bucket {
		return errors.New("storage bucket is not configured")
	}
	lock, err := os.OpenFile(filepath.Join(u.dir, "upload.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return ErrUploadBusy
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	object, chunks, err := u.meta.GetObject(ctx, bucket, key)
	if errors.Is(err, metadata.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if expectedETag != "" && object.ETag != expectedETag {
		return errors.New("Telegram object was replaced; cleanup retained for inspection")
	}
	if u.deleteMessage == nil {
		return errors.New("Telegram message deletion is not configured")
	}
	binding, err := u.meta.GetBucket(ctx, bucket)
	if err != nil {
		return err
	}
	seen := map[int64]bool{}
	for _, chunk := range chunks {
		id := chunk.TelegramMessageID
		if id <= 0 {
			return errors.New("Telegram chunk has no message ID")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		var shared int
		if err := u.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM object_chunks WHERE bucket=? AND key!=? AND telegram_message_id=?`, bucket, key, id).Scan(&shared); err != nil {
			return err
		}
		if shared > 0 {
			continue
		}
		if err := u.deleteMessage(ctx, binding.ChatID, id); err != nil {
			return fmt.Errorf("delete Telegram chunk message %d: %w", id, err)
		}
	}
	return u.meta.DeleteObject(ctx, bucket, key)
}

func telegramMessageDeleter(token, base string) func(context.Context, string, int64) error {
	client := &http.Client{Timeout: 20 * time.Second}
	return func(ctx context.Context, chat string, id int64) error {
		form := url.Values{"chat_id": {chat}, "message_id": {strconv.FormatInt(id, 10)}}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(base, "/")+"/bot"+token+"/deleteMessage", strings.NewReader(form.Encode()))
		if err != nil {
			return errors.New("invalid Telegram deletion endpoint")
		}
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response, err := client.Do(request)
		// HTTP errors can include a credential-bearing URL. Do not persist that URL.
		if err != nil {
			return errors.New("Telegram deletion request failed")
		}
		defer response.Body.Close()
		var result struct {
			OK          bool   `json:"ok"`
			Description string `json:"description"`
			Result      bool   `json:"result"`
		}
		if err := json.NewDecoder(io.LimitReader(response.Body, 64<<10)).Decode(&result); err != nil {
			return errors.New("invalid Telegram deletion response")
		}
		if response.StatusCode == 200 && result.OK && result.Result {
			return nil
		}
		if response.StatusCode == 400 && strings.Contains(strings.ToLower(result.Description), "message to delete not found") {
			return nil
		}
		return fmt.Errorf("Telegram deletion refused (HTTP %d): %s", response.StatusCode, result.Description)
	}
}
