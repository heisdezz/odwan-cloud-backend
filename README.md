# Media library server

A Go application built on PocketBase for storing media in Telegram or S3-compatible storage and streaming it through a media library API. Telegram storage embeds [TgNAS](https://github.com/aahl/tgnas); S3 storage uses the AWS SDK for Go v2. The server runs as one process without Docker.

## Features

- Creates the media, album, tag, and library statistics collections on startup.
- Maintains album counts when media records are created, updated, or deleted, and repairs counts on startup.
- Creates a protected default album named `unsorted`; album names are unique, ignoring case.
- Uploads files with SHA-256 verification, duplicate detection, and persistent resume checkpoints.
- Streams media with HTTP byte ranges for seeking, without merging Telegram chunks into a temporary file.
- Includes a browser test viewer for images, video, audio, and downloads.

## Quick start

Use the Go version declared in [go.mod](go.mod), currently **1.27.1**. File locking uses Unix APIs; Linux and macOS are the intended environments.

On a fresh checkout, create your configuration:

```bash
cp config.json.example config.json
```

Edit `config.json` with your storage credentials, then run from the project directory:

```bash
go mod download
go run . serve --dir=./pb_data --http=127.0.0.1:8090
```

The example configuration selects Telegram. Fill in `telegram.bot_token` and `telegram.chat_id` before starting.

| URL | Purpose |
| --- | --- |
| `http://localhost:8090/_/` | PocketBase dashboard |
| `http://localhost:8090/api/` | PocketBase REST API |
| `http://localhost:8090/api/test/media/view` | Test upload viewer |

Follow PocketBase's startup instructions to create the first superuser. The test viewer uses the configured test token, independently of dashboard authentication.

To build and run a binary:

```bash
mkdir -p bin
go build -o bin/media-library .
./bin/media-library serve --dir=./pb_data --http=127.0.0.1:8090
```

Run the binary from the directory containing `config.json`. The explicit `--dir` keeps the PocketBase database in the same location for both commands.

## Configuration

The server loads `./config.json` automatically, relative to its working directory. Settings and credentials come from JSON; environment variables do not override them. Restart after changing the file. Missing or malformed configuration stops startup.

### Telegram

A minimal Telegram configuration is:

```json
{
  "storage_backend": "telegram",
  "storage_path": "./storage",
  "test_token": "replace-with-your-test-token",
  "telegram": {
    "bot_token": "YOUR_BOT_TOKEN",
    "chat_id": "YOUR_CHAT_ID",
    "bucket": "telegram",
    "chunk_size": 8388608,
    "max_file_size": 268435456
  }
}
```

Use a destination chat ID recognized by the Bot API. `chat_id: "me"` is unsupported. Telegram app IDs and app hashes are not required by this Bot API integration.

Uploads run one at a time. The default chunk size is 8 MiB; the supported maximum is 20 MiB. All sizes are numeric byte counts, not expressions such as `"8 * 1024 * 1024"`.

Single-document uploads retain the object filename and extension. Larger files become documents such as `video.mp4.part000001`; the application exposes them as one logical file. Individual parts need the application to reassemble their byte stream.

The adapter disables automatic Telegram content type detection and handles video, audio, or animation acknowledgements as well as document acknowledgements. Acknowledged chunk IDs are saved in SQLite for retries and streaming.

### S3-compatible storage

Set `storage_backend` to `"s3"` and configure:

```json
{
  "storage_backend": "s3",
  "storage_path": "./storage",
  "test_token": "replace-with-your-test-token",
  "s3": {
    "endpoint": "https://YOUR_S3_ENDPOINT",
    "region": "YOUR_REGION",
    "bucket": "YOUR_BUCKET",
    "part_size": 8388608,
    "concurrency": 4
  },
  "key_data": {
    "keyID": "YOUR_ACCESS_KEY_ID",
    "applicationKey": "YOUR_SECRET_ACCESS_KEY"
  }
}
```

S3 multipart uploads use parallel parts. `concurrency` supports 1–32 workers; `part_size` supports 5 MiB–5 GiB. Files smaller than one part do not benefit from parallel part uploads. Credentials must permit reading object metadata as well as uploading.

Both backend sections may coexist in one configuration. `storage_backend` selects the upload destination; streaming selects the backend stored on each media record. Each record's bucket must match that backend's configured bucket. If `storage_backend` is omitted, it defaults to `"s3"`.

See [S3 details](s3/README.md) and [Telegram details](telegramstorage/README.md).

## Connection test

`GET /api/test/connection` returns HTTP 200 with the plain-text body `ok`. It requires no authentication and works when `test_token` is empty.

```bash
curl http://localhost:8090/api/test/connection
```

## Uploading files

The test endpoint is available when `test_token` is nonempty:

```bash
curl --fail-with-body \
  -H 'X-S3-Test-Token: replace-with-your-test-token' \
  -F 'file=@/path/to/video.mp4' \
  'http://localhost:8090/api/test/s3/upload'
```

The endpoint name is shared by both storage backends.

| Input | Meaning |
| --- | --- |
| `X-S3-Test-Token` header | Must match `test_token` in the configuration |
| Multipart `file` field | Exactly one file |
| Optional multipart `hash` field | The file's 64-character SHA-256 hexadecimal digest |
| Optional `key` query parameter | Destination object key; defaults to `tests/<filename>` |

Keys must be clean paths under `tests/`, for example `?key=tests/video.mp4`. Requests are limited to **256 MiB including multipart overhead**, even if the backend permits larger files.

The server calculates the hash before uploading and verifies any supplied hash. Identical indexed content returns the existing key and media record without another cloud transfer.

An illustrative response:

```json
{
  "backend": "telegram",
  "bucket": "telegram",
  "key": "tests/video.mp4",
  "etag": "<storage-etag>",
  "size_bytes": 123456,
  "hash": "<sha256-hex-digest>",
  "status": "success",
  "duplicate": false,
  "media_id": "<record-id>",
  "stream_url": "/api/media/<record-id>/stream",
  "view_url": "/api/test/media/view?id=<record-id>"
}
```

Duplicate responses use `status: "duplicate"` and `duplicate: true`. When a duplicate is submitted under another key, the response points to the existing object.

After an interruption, resend the same file and key. The HTTP request still sends the entire file to this server; resumption and deduplication save the **server-to-storage** transfer. Completed Telegram chunks and S3 parts survive restarts. If a Telegram upload was accepted but its acknowledgement was lost, a retry may post that chunk again.

Successful uploads create or update `media_item`. An existing record with the same hash and no storage mapping is adopted, preserving its album and original path. New records go into `unsorted`. If storage succeeds but the database save fails, retry the upload to repair the mapping. Files uploaded through this adapter before database integration can also be linked by resubmitting them.

### Yaak

1. Create a `POST` request to `http://localhost:8090/api/test/s3/upload`.
2. Add the `X-S3-Test-Token` header.
3. Choose a multipart form body and add a **file** field named `file`.
4. Optionally add a **text** field named `hash`.
5. Send the request and open its returned `view_url` in a browser.

Let Yaak generate the multipart `Content-Type` header and boundary.

## Viewing and streaming

Open **[http://localhost:8090/api/test/media/view](http://localhost:8090/api/test/media/view)**, enter your test token, and select an upload. The page supports image previews, video/audio controls, seeking, pagination, and downloading originals.

The viewer uses a signed, HttpOnly, SameSite cookie valid for one hour. It only lists and streams completed files under `tests/`; reconnect when the session expires. Set `test_token` to `""` to disable test uploads and the viewer.

For direct streaming:

```bash
curl --fail-with-body \
  -H 'X-S3-Test-Token: replace-with-your-test-token' \
  -H 'Range: bytes=0-1048575' \
  'http://localhost:8090/api/media/RECORD_ID/stream' \
  -o first-part.bin
```

`GET /api/media/{id}/stream` streams the logical file. `HEAD` returns metadata without downloading content. Single ranges support fixed, open-ended, and suffix offsets. Partial responses use HTTP 206 and `Content-Range`; unsatisfiable or multiple ranges return HTTP 416. ETag and modification-time validators are supported.

Telegram downloads only overlapping chunks and streams their bytes in order. It may discard a prefix within the first requested chunk. Memory use does not grow with the full file size, though playback can buffer while the next Telegram request starts. Browser playback depends on the source codec and file layout.

### Access rules and browser playback

Normal streams follow the `media_item` collection's **View rule**. PocketBase authentication through the `Authorization` header is supported. The test header grants access only to objects under `tests/`; the viewer cookie only authorizes its separate test routes.

For a browser player with an authenticated PocketBase JavaScript client:

```js
const token = await pb.files.getToken();
const video = document.querySelector("video");
video.src = `${pb.baseURL}/api/media/${media.id}/stream?token=${encodeURIComponent(token)}`;
video.controls = true;
```

File tokens are short-lived and still require the View rule to permit access. Refresh them for later playback or seek requests after expiry. Regular auth tokens and test tokens are not accepted as stream URL parameters. Media allowed by a public View rule can use the stream URL directly.

See [streaming and viewer details](routes/README.md).

## PocketBase collections

| Collection | Purpose |
| --- | --- |
| `media_item` | File hashes, paths, MIME types, upload status, album relation, and storage mapping |
| `album` | Unique album names, paths, cover media, and maintained media counts |
| `tag` | Tag names, colors, categories, and media count fields |
| `media_tag` | Unique media/tag relation pairs |
| `library_stats` | Schema for library totals and database statistics |

Startup adds missing storage fields to existing media collections: `storage_backend`, `storage_bucket`, `storage_key`, and `storage_etag`. A unique index prevents multiple records pointing to the same backend/bucket/key. Existing records and API rules are preserved. Collection creation does not grant public access automatically.

The `unsorted` album has both name and ID `unsorted`, cannot be renamed or deleted, and receives media with no album. Album counts update transactionally and are repaired on startup. PocketBase IDs and relation IDs are strings; [models.ts](models.ts) reflects this.

## Data and backups

Keep the PocketBase data directory and the configured storage directory across restarts:

| Path | Contents |
| --- | --- |
| `pb_data/` with the commands above | PocketBase database, settings, and internal application data |
| `<storage_path>/.telegram/` | Telegram object/chunk mappings and resume metadata |
| `<storage_path>/.s3-uploads/` | S3 multipart checkpoints |
| `<storage_path>/.s3-hashes/` | Persistent S3 duplicate index |
| `<storage_path>/.s3-test-inputs/` | Temporary HTTP upload files, removed after requests |

Back up both application data and storage metadata. Telegram messages alone do not preserve the logical file mapping. Keep `config.json` with your server configuration; it contains credentials and is ignored by Git. The example file contains placeholders.

## S3 upload CLI

The CLI always uses S3, regardless of `storage_backend`, and uploads directly without creating PocketBase media records:

```bash
go run ./cmd/s3-upload \
  -config config.json \
  -file /path/to/video.mp4 \
  -key media/video.mp4
```

Repeat the command to resume, or abort a saved multipart upload:

```bash
go run ./cmd/s3-upload -config config.json -abort -key media/video.mp4
```

The CLI retains overwrite behavior. The HTTP upload endpoint uses duplicate detection.

## Frontend integration

See [IMPLEMENTATION.md](IMPLEMENTATION.md) for custom API contracts, upload states, playback authentication, record normalization, server hooks, and backend features still needed by a full frontend.

## Development

```bash
go test ./...
go vet ./...
go test -race ./routes ./telegramstorage ./s3 ./init
```

Automated tests use temporary databases and fake storage clients; they do not upload to your live Telegram chat or S3 bucket.

| Directory | Responsibility |
| --- | --- |
| `init/` | Collection setup, upgrades, default album, and album count hooks |
| `routes/` | Upload API, record integration, streaming, and test viewer |
| `telegramstorage/` | Embedded TgNAS adapter, resumable chunks, and Telegram response handling |
| `s3/` | Configuration, parallel multipart uploads, deduplication, and streaming |
| `cmd/s3-upload/` | Standalone S3 upload CLI |

## Troubleshooting

| Symptom | Check |
| --- | --- |
| Test upload/viewer returns 404 | Set a nonempty `test_token`, restart, and use the documented route and method |
| Upload or viewer returns 401 | Supply the configured test token, or reconnect if the viewing cookie expired |
| Normal stream returns 404 | Check the record ID, View rule, and authentication; denied access is returned as 404 |
| Stream returns 409 | Complete the upload or resubmit an older upload to populate its storage mapping |
| Upload returns 400 | Check the multipart file field, supplied hash, and `tests/` key |
| Upload returns 413 | Reduce the request size below 256 MiB including form overhead, and check Telegram's configured file-size limit |
| Telegram upload returns 409 | Another transfer is active; retry after it finishes |
| Upload returns 502 | Inspect the server log for the storage error, then retry the same unchanged file and key |
| Video loads but cannot play | Check browser codec support and try downloading the original |
| Startup fails while upgrading album names | Resolve duplicate names, including case-only duplicates, before restarting |

## Thumbnails

Install FFmpeg (`sudo apt install ffmpeg` on Ubuntu). Startup adds a public `thumbs` file field to `media_item`. `GET /api/media/{id}/thumb` creates a missing image/video preview on demand and caches the JPEG in PocketBase storage. Thumbnail URLs require no token. Original streaming keeps its existing permissions. The test viewer loads video posters through `/api/test/media/{id}/thumb`.

Generation uses one worker, one FFmpeg thread, a 320×320 size limit, and a 45-second timeout. Concurrent uncached requests return 503 with `Retry-After: 2`; failed files have a five-minute retry delay. Cached files need no decoder or remote reads. Split Telegram videos are read using byte ranges, without a merged temporary video. Source changes invalidate the thumbnail; no cron job is needed. See [IMPLEMENTATION.md](IMPLEMENTATION.md#on-demand-thumbnails) for frontend integration and retry behavior.

## Telegram deletion

Deleting a `media_item` queues deletion of its Telegram chunk messages after the database transaction commits. A single background worker processes the persistent queue immediately and polls every minute, with retries on failure. Thumbnail files are removed through PocketBase's file lifecycle. No external cron is needed.

Telegram Bot API messages must be less than 48 hours old to be deleted. Failed cleanup keeps its metadata and appears in logs and the internal `_media_storage_deletions` table. See [Telegram's restrictions](https://core.telegram.org/bots/api#deletemessage) and [the frontend guide](IMPLEMENTATION.md#telegram-cleanup-after-deletion). The queue also supports S3 object deletion. Versioned buckets can retain older versions.

## Trash and restore

Authenticated `GET /api/media/capabilities` advertises Trash support. Use `POST /api/media/{id}/trash`, `POST /api/media/{id}/restore`, and `DELETE /api/media/{id}/permanent` with your PocketBase auth token. Trash is recoverable for 30 days and is excluded from album totals; original streaming is disabled while trashed. Permanent deletion and expired-trash cleanup queue cloud deletion. See [the frontend contract](IMPLEMENTATION.md#trash-api-and-capability-detection).
