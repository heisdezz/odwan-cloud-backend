Telegram storage embeds the pinned `aahl/tgnas` Go package. No Docker or separate TgNAS server is needed.

Set `storage_backend` to `telegram` in `config.json`, with:

```json
{
  "storage_backend": "telegram",
  "storage_path": "./storage",
  "test_token": "local-test",
  "telegram": {
    "bot_token": "YOUR_BOT_TOKEN",
    "chat_id": "YOUR_CHAT_ID",
    "bucket": "telegram",
    "chunk_size": 8388608,
    "max_file_size": 268435456
  }
}
```

Restart the app. POST multipart form data to `/api/test/s3/upload` with header `X-S3-Test-Token: local-test` and a file field named `file`. An optional text field `hash` accepts a SHA-256 hexadecimal hash and is verified before transfer. Without it, the server calculates the hash. Optional query parameter `key` must start with `tests/`.

The response includes `backend: "telegram"`, the calculated hash, and `status: "success"` or `"duplicate"`. A duplicate returns the existing object's key without transferring the file again. Previously uploaded documents outside this adapter are not indexed.

Uploads run one at a time; overlapping transfers return HTTP 409. Single-document uploads use the object filename, including its extension. Larger files use filenames such as `video.mp4.part000001`; these chunks need reassembly through the adapter. Existing Telegram messages keep their previous names, and deduplication will still skip them. Files are split into documents of at most 20 MiB, preserving their bytes. After an interruption, resend the same file; acknowledged chunks survive app restarts and are skipped. An upload whose Telegram acknowledgement was lost may resend that chunk. HTTP requests are limited to 256 MiB including multipart overhead.

Keep and back up `storage_path/.telegram/`: its SQLite database maps files to Telegram document IDs and contains resume checkpoints. Telegram documents alone do not retain the logical file map. Streaming is available at `/api/media/{media_id}/stream`. See [streaming and PocketBase integration](../routes/README.md).

Use `storage_backend: "s3"` to select the existing Backblaze uploader. The S3 CLI continues to use S3.

Document uploads explicitly disable Telegram content type detection. If Telegram still acknowledges a video, audio, or animation instead of a document, the adapter preserves its file ID so the accepted upload is recorded rather than retried.
