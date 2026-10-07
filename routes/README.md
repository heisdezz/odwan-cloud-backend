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

Access follows `media_item`'s **View rule**. A locked collection remains locked. Ordinary PocketBase authentication works through the `Authorization` header. For browser `<video>` or `<audio>` elements, use a short-lived PocketBase file token:

```js
// pb is your authenticated PocketBase client.
const token = await pb.files.getToken();
const video = document.querySelector("video");
video.src = `${pb.baseURL}/api/media/${media.id}/stream?token=${encodeURIComponent(token)}`;
video.controls = true;
```

The user's View rule must allow access to the record; a file token does not bypass it. Refresh expired tokens for later playback/seek requests. Regular auth tokens are not accepted in the URL. Public media can use the stream URL directly if its View rule allows anonymous access.

For Yaak, use the returned `stream_url` with `X-S3-Test-Token` and optionally `Range: bytes=0-1048575`. This test-token access is restricted to storage keys under `tests/`. The test token is not accepted as a URL query parameter.

Streaming preserves the media MIME type and original filename. Browser playback still depends on supported codecs and the source file's layout; a video whose metadata is at its end may require a seek before playback starts. Large streaming responses are not written to a temporary merged file.

Open **`http://localhost:8090/api/test/media/view`** to view test uploads in a browser. Enter `test_token` from `config.json` once, then select a file. The page previews images and uses native video/audio controls with seeking. It offers a download link for every file and paginates the upload list. Upload responses also include `view_url` with the record selected.

The viewer uses a one-hour signed, HttpOnly, SameSite viewing cookie scoped to `/api/test/media`. It lists and streams only completed objects under `tests/`, and is enabled only when `test_token` is configured. It does not change the normal media streaming endpoint's access rules. If your session expires, reconnect on the page.
