Successful test uploads now create or update a PocketBase `media_item` record and return:

```json
{
  "media_id": "RECORD_ID",
  "stream_url": "/api/media/RECORD_ID/stream",
  "backend": "telegram",
  "status": "success"
}
```

On startup, existing `media_item` collections gain optional `storage_backend`, `storage_bucket`, `storage_key`, and `storage_etag` fields. Existing records and API rules are preserved. An index prevents multiple records pointing at the same storage object. The uploader adopts an existing record with the same hash and no storage mapping; otherwise, new records go into the `unsorted` album. Repeating an upload returns the same record ID and keeps album counts correct.

Files uploaded before this integration can be linked by sending them through the upload endpoint again. Telegram deduplication skips the transfer and creates the missing database record. If storage succeeds but a database save fails, retrying the request repairs the mapping. Existing records without storage mappings return HTTP 409 rather than guessing a storage key.

`GET /api/media/{id}/stream` delivers the original logical file. `HEAD` returns metadata without downloading content. Both Telegram and S3 are supported; the record's backend selects the reader, and its bucket must match that backend's configuration. Streaming remains available when `test_token` is empty.

Single HTTP byte ranges are supported, including suffix ranges and open-ended ranges. Responses include `Accept-Ranges`, `Content-Length`, and, for partial content, HTTP 206 with `Content-Range`. Unsatisfiable or multiple ranges return HTTP 416. ETag, Last-Modified, If-None-Match, If-Modified-Since, and If-Range are supported. Telegram fetches only the chunks overlapping the requested range and streams them in order; it may discard a prefix within the first chunk. There is no full-file merge or buffering. Individual `.partNNNNNN` documents are not standalone playable videos.

Streams are public and require no token. Use the returned `stream_url` directly with native image, video, or audio elements; range requests also require no token. Uploads keep their `X-S3-Test-Token` requirement. Trashed originals return 404.

Streaming preserves the media MIME type and original filename. Browser playback still depends on supported codecs and the source file's layout; a video whose metadata is at its end may require a seek before playback starts. Large streaming responses are not written to a temporary merged file.

Open **`http://localhost:8090/api/test/media/view`** to view test uploads directly, with no login form or viewing session. Test viewing routes remain available when the upload test token is empty. The viewer lists active completed `tests/` objects and supports playback, posters, pagination, and downloads.

`GET /api/media/{id}/thumb` generates a missing image/video JPEG and stores it in the public `media_item.thumbs` file field. Thumbnail viewing and generation do not require a token. Original streams are public too. Test uploads can use `/api/test/media/{id}/thumb` without a token. `HEAD` never generates files. One generation runs at a time; HTTP 503 includes `Retry-After` for busy or failed jobs. FFmpeg must be installed. See [the frontend guide](../IMPLEMENTATION.md#on-demand-thumbnails) for caching, invalidation, and retries.
