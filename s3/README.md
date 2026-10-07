The test endpoint supports `storage_backend: "s3"` or `"telegram"`. See [Telegram configuration](../telegramstorage/README.md) for TgNAS uploads. The CLI described below always uses S3.

# Resumable S3 uploads

Uses the AWS Go SDK v2 with Backblaze B2's S3-compatible endpoint. Configuration mirrors the `storage_path` and `s3` shape in `config.ts`; Go reads JSON, not TypeScript. The server and CLI automatically load `./config.json` at startup, relative to the working directory. Use the CLI's `-config` flag to select another file. Missing or invalid configuration files produce an error. `part_size` is a number of bytes, for example `8388608` for 8 MiB.

```bash
cp config.json.example config.json
# Set key_data.keyID and key_data.applicationKey in config.json.
go run ./cmd/s3-upload -config config.json -file /path/to/video.mp4 -key media/video.mp4
```

Run the same command after a failure or Ctrl+C to resume. Completed parts are discovered from S3, including parts whose responses were lost. Changing the configured part size does not change an upload already in progress. Uploads use bounded memory and parallel parts, with retries handled by the SDK. An existing object at the destination key is replaced when the upload completes.

```bash
go run ./cmd/s3-upload -config config.json -abort -key media/video.mp4
```

All settings come from JSON; environment variables do not override them. Credentials come from `key_data.keyID` and `key_data.applicationKey` and are never written into resume state. The `s3` section supplies the endpoint, region, bucket, and part size. Keep application keys on the server; do not import `s3_key.ts` into browser code.

Go usage:

```go
cfg, err := s3.LoadConfig("config.json")
if err != nil { return err }
uploader, err := s3.New(cfg)
if err != nil { return err }
result, err := uploader.UploadFile(ctx, localPath, objectKey, func(p s3.Progress) {
    log.Printf("%.1f%%", p.Percentage)
})
```

Resume checkpoints live in `<storage_path>/.s3-uploads`. Keep this directory across restarts. File locks prevent concurrent uploads to the same object on Linux/macOS and are released when a process exits. The source is SHA-256 checked before upload/resume and again before completion; keep it unchanged while uploading. Cancellation retains incomplete remote parts until resumed or explicitly aborted. If a server lifecycle rule expires an incomplete upload, abort the local state before starting again. Empty files use a single PUT.

Progress counts bytes acknowledged by the server, not bytes currently in transit. Upload success is indicated by a nil error. A saved token allows recovery when completion succeeds but its response is lost. A process crash between creating a multipart upload and saving its ID can leave orphan parts; use a bucket lifecycle rule to expire abandoned multipart uploads.

Tests use a fake S3 service and never upload to the live bucket.

## HTTP test endpoint

Enable the endpoint when starting PocketBase:

```bash
# Set key_data.keyID and key_data.applicationKey in config.json.
# Set test_token in config.json (example: local-test).
go run . serve --http=127.0.0.1:8090
```

From a second terminal, use the same token as `test_token` in config.json:

```bash
curl --fail-with-body \
  -H 'X-S3-Test-Token: local-test' \
  -F 'file=@/tmp/s3-test.bin' \
  'http://127.0.0.1:8090/api/test/s3/upload?key=tests/s3-test.bin'
```

Returns JSON with `status`, `bucket`, `key`, `etag`, `size_bytes`, `hash` (SHA-256), and `duplicate`. The `key` query parameter is optional and defaults to `tests/<filename>`. Keys must stay under `tests/`. Requests are limited to 256 MiB including multipart overhead and accept exactly one `file` field. This endpoint uploads to the configured live bucket.

After an S3 transfer interruption, resend the same file and key to resume the S3 multipart upload. The HTTP request sends the entire local file again; resumption applies to the server-to-S3 transfer. Temporary local files are removed after each request, while the uploader's checkpoint remains on failure. The response arrives after completion; progress is available through the CLI instead.

The endpoint is registered only when `test_token` is set in config.json. It requires that token in `X-S3-Test-Token` and uses the same JSON credentials as the CLI. Restart the server after changing config.json.


## Parallel uploads and duplicate detection

Set `s3.concurrency` in config.json to control simultaneous part uploads (default 4, supported range 1–32). `s3.part_size` remains 8 MiB by default; files smaller than one part cannot benefit from parallel part uploads. Workers stream file sections, SDK retries remain enabled, completion orders parts by part number, and interrupted uploads remain resumable. Progress callbacks run serially.

The test endpoint accepts an optional multipart text field `hash` containing the file's 64-character SHA-256 hex digest. If absent, the server generates it before contacting S3. A supplied hash is verified against the actual bytes; malformed or mismatched hashes return HTTP 400. For example:

```bash
curl --fail-with-body \
  -H 'X-S3-Test-Token: local-test' \
  -F 'file=@/tmp/s3-test.bin' \
  -F "hash=$(sha256sum /tmp/s3-test.bin | cut -d ' ' -f 1)" \
  'http://127.0.0.1:8090/api/test/s3/upload?key=tests/s3-test.bin'
```

A persistent, per-bucket index lives under `<storage_path>/.s3-hashes`. The server verifies the indexed object still exists with matching SHA-256 metadata and size before skipping an upload. Identical content sent under another filename or key returns `status: "duplicate"`, `duplicate: true`, and the existing object's key; it does not create another S3 object. Missing/deleted objects are uploaded again. Hash locks prevent simultaneous requests for the same content from sending duplicate transfers; a competing active request receives an upload error and can be retried. Keep the hash index across restarts. S3 credentials need permission to read object metadata as well as upload.

The check avoids retransmission from the server to S3. The multipart HTTP request still sends the file to the server. Objects uploaded before this index existed can be recognized at their requested key if they carry the uploader's SHA-256 metadata; there is no full-bucket scan.

Use `UploadFileUnique(ctx, localPath, key, optionalHash, progress)` for the same deduplication behavior from Go. `UploadFile` and the CLI retain their original overwrite behavior.
