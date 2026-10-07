// Package telegramstorage embeds TgNAS and persists acknowledged upload chunks.
package telegramstorage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/aahl/tgnas/metadata"
	"github.com/aahl/tgnas/store"
	"github.com/aahl/tgnas/telegram"
	"go-test/s3"
)

var ErrUploadBusy = errors.New("another Telegram upload is active; retry when it finishes")
var ErrTooLarge = errors.New("file exceeds telegram.max_file_size")

const objectStrategy = "pocketbase-telegram"

type Uploader struct {
	meta                   *metadata.SQLiteStore
	objects                *store.ObjectStore
	db                     *sql.DB
	dir, bucket            string
	chunkSize, maxFileSize int64
}

func New(c s3.Config) (*Uploader, error) {
	base := c.Telegram.APIBaseURL
	if base == "" {
		base = "https://api.telegram.org"
	}
	client := telegram.NewHTTPClient(c.Telegram.BotToken, base, &http.Client{Timeout: 5 * time.Minute, Transport: documentTransport{}})
	return newUploader(c, client)
}

func newUploader(c s3.Config, client telegram.Client) (*Uploader, error) {
	cfg := c.Telegram
	if strings.TrimSpace(c.StoragePath) == "" || cfg.BotToken == "" || cfg.ChatID == "" {
		return nil, fmt.Errorf("storage_path, telegram.bot_token and telegram.chat_id are required")
	}
	if cfg.ChatID == "me" {
		return nil, fmt.Errorf("telegram.chat_id must be a Bot API chat ID, not me")
	}
	if cfg.Bucket == "" {
		cfg.Bucket = "telegram"
	}
	if cfg.ChunkSize == 0 {
		cfg.ChunkSize = 8 << 20
	}
	if cfg.MaxFileSize == 0 {
		cfg.MaxFileSize = 256 << 20
	}
	if cfg.ChunkSize < 1 || cfg.ChunkSize > 20<<20 {
		return nil, fmt.Errorf("telegram.chunk_size must be between 1 byte and 20 MiB")
	}
	if cfg.MaxFileSize < 1 {
		return nil, fmt.Errorf("telegram.max_file_size must be positive")
	}
	// Keep metadata separate for each bot, chat and bucket; rotating a token for
	// the same bot retains access to its existing metadata.
	botID, _, _ := strings.Cut(cfg.BotToken, ":")
	scope := sha256.Sum256([]byte(botID + "\x00" + cfg.ChatID + "\x00" + cfg.Bucket))
	dir := filepath.Join(c.StoragePath, ".telegram", hex.EncodeToString(scope[:]))
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	dbPath := filepath.Join(dir, "metadata.sqlite")
	meta, err := metadata.OpenSQLite(dbPath)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*Uploader, error) { meta.Close(); return nil, err }
	if err := meta.UpsertBucket(context.Background(), metadata.Bucket{Name: cfg.Bucket, ChatID: cfg.ChatID, CreatedAt: time.Now().UTC(), Enabled: true}); err != nil {
		return fail(err)
	}
	uploadConfig := store.DefaultUploadConfig()
	uploadConfig.Strategy = "document"
	uploadConfig.MaxFileSize = cfg.MaxFileSize
	uploadConfig.ChunkSize = cfg.ChunkSize
	uploadConfig.TypeLimits[telegram.TypeDocument] = cfg.ChunkSize
	objects, err := store.NewObjectStore(meta, documentNamingClient{client}, store.Options{Upload: uploadConfig, MaxUploads: 1, MaxDownloads: 1, MaxTelegramCalls: 1})
	if err != nil {
		return fail(err)
	}
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return fail(err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS pb_telegram_hash ON objects(bucket,sha256,size,upload_strategy);
 CREATE TABLE IF NOT EXISTS pb_telegram_uploads(hash TEXT PRIMARY KEY,size INTEGER NOT NULL,chunk_size INTEGER NOT NULL);`); err != nil {
		db.Close()
		return fail(err)
	}
	return &Uploader{meta: meta, objects: objects, db: db, dir: dir, bucket: cfg.Bucket, chunkSize: cfg.ChunkSize, maxFileSize: cfg.MaxFileSize}, nil
}

func (u *Uploader) Close() error { return errors.Join(u.db.Close(), u.meta.Close()) }

func (u *Uploader) UploadFileUnique(ctx context.Context, filename, key, expectedHash string, progress func(s3.Progress)) (s3.Result, error) {
	result := s3.Result{Backend: "telegram", Bucket: u.bucket, Key: key}
	expectedHash = strings.ToLower(strings.TrimSpace(expectedHash))
	if expectedHash != "" {
		decoded, err := hex.DecodeString(expectedHash)
		if err != nil || len(decoded) != 32 {
			return result, s3.ErrInvalidHash
		}
	}
	if key == "" || strings.HasPrefix(key, ".pending/") {
		return result, fmt.Errorf("invalid object key")
	}
	file, err := os.Open(filename)
	if err != nil {
		return result, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() {
		return result, fmt.Errorf("source must be a regular file")
	}
	if info.Size() > u.maxFileSize {
		return result, ErrTooLarge
	}
	hash, err := fileHash(ctx, file)
	if err != nil {
		return result, err
	}
	if expectedHash != "" && expectedHash != hash {
		return result, s3.ErrHashMismatch
	}
	result.Hash = hash
	lock, err := os.OpenFile(filepath.Join(u.dir, "upload.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return result, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return result, ErrUploadBusy
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if err := ctx.Err(); err != nil {
		return result, err
	}
	var existingKey, etag string
	err = u.db.QueryRowContext(ctx, `SELECT key,etag FROM objects WHERE bucket=? AND sha256=? AND size=? AND upload_strategy=? ORDER BY key LIMIT 1`, u.bucket, hash, info.Size(), objectStrategy).Scan(&existingKey, &etag)
	if err == nil {
		result.Key = existingKey
		result.ETag = etag
		result.Duplicate = true
		if progress != nil {
			progress(s3.Progress{BytesWritten: info.Size(), ContentLength: info.Size(), Percentage: 100})
		}
		return result, u.cleanup(ctx, hash)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return result, err
	}
	if _, err := u.db.ExecContext(ctx, `INSERT OR IGNORE INTO pb_telegram_uploads(hash,size,chunk_size) VALUES(?,?,?)`, hash, info.Size(), u.chunkSize); err != nil {
		return result, err
	}
	var size, chunkSize int64
	if err := u.db.QueryRowContext(ctx, `SELECT size,chunk_size FROM pb_telegram_uploads WHERE hash=?`, hash).Scan(&size, &chunkSize); err != nil {
		return result, err
	}
	if size != info.Size() || chunkSize < 1 || chunkSize > 20<<20 {
		return result, fmt.Errorf("invalid Telegram upload checkpoint")
	}
	var sniff [512]byte
	n, _ := file.ReadAt(sniff[:], 0)
	contentType := http.DetectContentType(sniff[:n])
	chunks := []metadata.Chunk{}
	var written int64
	count := (size + chunkSize - 1) / chunkSize
	for part := int64(0); part < count; part++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		offset := part * chunkSize
		length := min(chunkSize, size-offset)
		partKey := fmt.Sprintf(".pending/%s/%06d", hash, part+1)
		object, refs, err := u.meta.GetObject(ctx, u.bucket, partKey)
		if errors.Is(err, metadata.ErrNotFound) || (err == nil && object.Size != length) {
			// TgNAS stores each acknowledged part and its Telegram references in SQLite.
			// Using document mode preserves the original bytes.
			documentName := path.Base(key)
			documentType := contentType
			if count > 1 {
				// Individual chunks are partial data, not independently usable media.
				documentName = fmt.Sprintf("%s.part%06d", documentName, part+1)
				documentType = "application/octet-stream"
			}
			uploadContext := context.WithValue(ctx, documentNameKey{}, documentName)
			_, err = u.objects.PutObject(uploadContext, store.PutObjectInput{Bucket: u.bucket, Key: partKey, ContentType: documentType, Size: length, Body: io.NewSectionReader(file, offset, length)})
			if err != nil {
				return result, fmt.Errorf("upload Telegram chunk %d (checkpoint retained): %w", part+1, err)
			}
			object, refs, err = u.meta.GetObject(ctx, u.bucket, partKey)
		}
		if err != nil {
			return result, err
		}
		var partBytes int64
		for _, ref := range refs {
			ref.Bucket = u.bucket
			ref.Key = key
			ref.PartNumber = len(chunks) + 1
			ref.Offset = written + partBytes
			partBytes += ref.Size
			chunks = append(chunks, ref)
		}
		if partBytes != length {
			return result, fmt.Errorf("invalid Telegram chunk metadata")
		}
		written += length
		if progress != nil {
			progress(s3.Progress{BytesWritten: written, ContentLength: size, Percentage: 100 * float64(written) / float64(size)})
		}
	}
	finalHash, err := fileHash(ctx, file)
	if err != nil {
		return result, err
	}
	if finalHash != hash {
		return result, s3.ErrHashMismatch
	}
	// Publish a single logical object pointing at acknowledged Telegram chunks.
	// Completion needs no additional Telegram upload and is committed atomically.
	object := metadata.Object{Bucket: u.bucket, Key: key, Size: size, ContentType: contentType, ETag: hash + "-sha256", SHA256: hash, LastModified: time.Now().UTC(), ChunkCount: len(chunks), TelegramType: telegram.TypeDocument, UploadStrategy: objectStrategy}
	if err := u.meta.PutObject(ctx, object, chunks); err != nil {
		return result, err
	}
	result.ETag = object.ETag
	if err := u.cleanup(ctx, hash); err != nil {
		return result, fmt.Errorf("upload completed but checkpoint cleanup failed: %w", err)
	}
	if progress != nil {
		progress(s3.Progress{BytesWritten: size, ContentLength: size, Percentage: 100})
	}
	return result, nil
}

func (u *Uploader) cleanup(ctx context.Context, hash string) error {
	tx, err := u.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	prefix := ".pending/" + hash + "/%"
	// Only remove temporary metadata. The final object owns the same Telegram
	// messages, so deleting remote messages here would destroy its content.
	for _, query := range []string{`DELETE FROM object_chunks WHERE bucket=? AND key LIKE ?`, `DELETE FROM objects WHERE bucket=? AND key LIKE ?`} {
		if _, err := tx.ExecContext(ctx, query, u.bucket, prefix); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM pb_telegram_uploads WHERE hash=?`, hash); err != nil {
		return err
	}
	return tx.Commit()
}

func fileHash(ctx context.Context, file *os.File) (string, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", err
	}
	hash := sha256.New()
	buffer := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := file.Read(buffer)
		hash.Write(buffer[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// GetObject opens the stored file through TgNAS; the caller must close the reader.
func (u *Uploader) GetObject(ctx context.Context, key string) (io.ReadCloser, store.ObjectInfo, error) {
	return u.objects.GetObject(ctx, store.GetObjectInput{Bucket: u.bucket, Key: key})
}

// Override Telegram's display filename without changing persistent checkpoint keys.
type documentNameKey struct{}
type documentNamingClient struct{ telegram.Client }

func (c documentNamingClient) Upload(ctx context.Context, request telegram.UploadRequest) (telegram.UploadedFile, error) {
	if name, ok := ctx.Value(documentNameKey{}).(string); ok {
		request.Filename = name
	}
	return c.Client.Upload(ctx, request)
}

func (u *Uploader) StatObject(ctx context.Context, bucket, key string) (s3.StreamInfo, error) {
	if bucket != u.bucket {
		return s3.StreamInfo{}, fmt.Errorf("storage bucket is not configured")
	}
	info, err := u.objects.HeadObject(ctx, bucket, key)
	if err != nil {
		return s3.StreamInfo{}, err
	}
	return s3.StreamInfo{Size: info.Size, ContentType: info.ContentType, ETag: info.ETag, Modified: info.LastModified}, nil
}

func (u *Uploader) OpenObject(ctx context.Context, bucket, key string, byteRange *s3.ByteRange) (io.ReadCloser, error) {
	if bucket != u.bucket {
		return nil, fmt.Errorf("storage bucket is not configured")
	}
	input := store.GetObjectInput{Bucket: bucket, Key: key}
	if byteRange != nil {
		input.Range = &store.ByteRange{Start: byteRange.Start, End: byteRange.End}
	}
	reader, _, err := u.objects.GetObject(ctx, input)
	return reader, err
}
