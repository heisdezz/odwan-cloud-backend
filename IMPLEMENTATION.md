# Frontend implementation guide

This document describes application behavior that is not apparent from PocketBase's collection editor: custom routes, upload semantics, streaming authentication, server hooks, and features that still need backend work. It reflects the implementation in this repository, rather than a proposed API.

Use [README.md](README.md) for running the server. Use the live dashboard for collection fields and API rules; use this guide for the frontend's behavior.

## 1. Integration boundaries

There are two API surfaces:

| Surface | Use it for |
| --- | --- |
| PocketBase collection API | Browsing records, albums, tags, editing library metadata, and authentication |
| Custom application routes | Transferring file bytes, linking uploads to records, and streaming cloud objects |

Original media lives in external storage; paths and storage fields point to that object. Creating a record through the standard collection API does not upload the original bytes. The public `thumbs` file field holds a generated JPEG preview. PocketBase file URLs apply to `thumbs`; use the custom stream route for the original.

Currently, the only HTTP upload route is a **test-token-authorized** route. A normal PocketBase user's auth token does not authorize it. Media browsing, streaming, thumbnails, and the test viewer are public.

### Custom route contract

| Method | Route | Authorization | Response |
| --- | --- | --- | --- |
| GET | `/api/test/connection` | None | HTTP 200, plain text `ok` |
| POST | `/api/test/s3/upload` | `X-S3-Test-Token` | Stored object information, record ID, stream/view URLs |
| GET, HEAD | `/api/media/{id}/thumb` | Public (no token) | Saved JPEG; GET generates it if missing |
| GET, HEAD | `/api/media/{id}/stream` | Public (no token) | Original file bytes or metadata |
| GET | `/api/test/media/view` | Public page shell; connect inside the page | Built-in browser viewer |
| GET | `/api/test/media?offset=0` | Public (no token) | Completed test uploads and `next_offset` |
| GET, HEAD | `/api/test/media/{id}/thumb` | Public (test objects only) | Test object thumbnail |
| GET, HEAD | `/api/test/media/{id}/stream` | Public (no token) | Test object's bytes or metadata |

Viewing routes are always registered, including when `test_token` is empty. Only the upload route is disabled when no test token is configured. Backend selection and credentials are server configuration, not frontend request fields.

## 2. Record shapes and derived values

[models.ts](models.ts) is a starting point, but distinguish a raw PocketBase record from a normalized frontend model.

| Value | Raw record behavior | Frontend handling |
| --- | --- | --- |
| IDs and relation IDs | Strings; default album ID is `unsorted` | Never convert IDs to numbers |
| Empty single relations | Usually `""`, rather than `null` | Normalize to `null` if your UI prefers it |
| Optional text fields | Usually `""` | Treat empty text as absent |
| `metadata_json` | Text, not a JSON field | Parse defensively; malformed/empty text must not break rendering |
| `duration_seconds` | Number field; no automatic media probing | Do not assume a meaningful duration was extracted |
| `album_name` | Not a stored field | Derive from expanded `album_id` or an album lookup |
| `tags` | Not a stored media field | Load through `media_tag`, then resolve `tag_id` |
| `storage_*` | Optional on legacy records | A record can exist without being streamable |
| `stream_url`, `view_url` | Upload-response fields, not collection fields | Derive a normal stream URL from the record ID when browsing |
| `media_count` on albums | Maintained by server hooks | Read it; do not calculate and write it from the client |

For a normalized media model, consider this readiness check:

```ts
function canStream(media: {
  upload_status: string;
  storage_backend?: string;
  storage_bucket?: string;
  storage_key?: string;
}) {
  return media.upload_status === "success" &&
    Boolean(media.storage_backend && media.storage_bucket && media.storage_key);
}
```

This indicates a complete mapping; storage can still be unavailable when playback starts. MIME types determine whether to render an image, video, audio element, or a download action. Always use the original logical media record, not individual Telegram chunk names.

### Browsing the library

Use normal PocketBase collection endpoints, subject to their configured List/View rules:

```http
GET /api/collections/media_item/records?page=1&perPage=50&sort=-created_at&expand=album_id
GET /api/collections/album/records?page=1&perPage=50&sort=name
GET /api/collections/tag/records?page=1&perPage=50&sort=name
```

With `expand=album_id`, the album is returned under `record.expand.album_id`; it does not become `record.album_name`. Relation expansion also requires permission to read the target record.

Filter an album's media with the collection API's `filter` query parameter, for example `album_id = "unsorted"`. Encode query parameters with `URLSearchParams`.

To resolve tags without relying on a flattened model:

1. Query `media_tag` with a filter such as `media_id = "RECORD_ID"` and `expand=tag_id`.
2. Read each join record's `expand.tag_id`.
3. Map those tag records into the frontend's `tags` array.

Creating a tag assignment means creating a `media_tag` record containing `media_id` and `tag_id`. Removing an assignment means deleting its join record. The `(media_id, tag_id)` pair is unique.

## 3. Upload flow

### Request

Send multipart form data containing exactly one file field named `file`. An optional text field named `hash` accepts a 64-character SHA-256 hexadecimal digest. The server computes and verifies the hash, so the frontend can omit it.

The optional `key` query parameter must be a clean object path under `tests/`. It defaults to `tests/<filename>`. Use a unique key for unrelated files: reusing the same key for different content can replace the stored object and update its existing media record.

```ts
interface UploadResult {
  backend: "telegram" | "s3";
  bucket: string;
  key: string;
  etag: string;
  size_bytes: number;
  hash: string;
  status: "success" | "duplicate";
  duplicate: boolean;
  media_id: string;
  stream_url: string;
  view_url: string;
}

async function uploadTestFile(
  baseURL: string,
  testToken: string,
  file: File,
  key: string,
  signal?: AbortSignal,
): Promise<UploadResult> {
  const url = new URL("/api/test/s3/upload", baseURL);
  url.searchParams.set("key", key);
  const form = new FormData();
  form.append("file", file);

  const response = await fetch(url, {
    method: "POST",
    headers: { "X-S3-Test-Token": testToken },
    body: form,
    signal,
  });
  const body = await response.json();
  if (!response.ok) {
    throw new Error(body.error ?? body.message ?? "Upload failed");
  }
  return body;
}

// Retain this key with the queue entry and reuse it on retry.
const key = `tests/${crypto.randomUUID()}-${file.name.replace(/[\\/]/g, "_")}`;
```

Let the browser set `Content-Type`; it must include the generated multipart boundary. The request limit is **256 MiB including multipart overhead**. Telegram's configured `max_file_size` may impose a smaller file limit. Leave room for the form wrapper when validating file size.

The shared test token is a privileged test credential, not a per-user login. Prompt for it in a developer/test tool rather than embedding it, Telegram credentials, or S3 credentials into a distributed frontend bundle.

### Success, duplicates, and database updates

Wait for the JSON response before marking an upload complete. A successful response means storage completed and the media record was saved.

The server:

1. Computes SHA-256 and checks duplicate content.
2. Transfers or resumes the object in the configured backend.
3. Finds the media record for that backend/bucket/key.
4. If needed, adopts a record with the same hash and no storage mapping, or creates one.
5. Writes the storage fields, size, MIME type, hash, and `upload_status: "success"`.

New records are assigned to `unsorted`. Adopted records keep their existing album and original path. The upload route does not accept an album selector, media record ID, tags, duration, or metadata JSON. To place a new upload into another album, update its returned `media_id` through PocketBase afterward:

```http
PATCH /api/collections/media_item/records/RECORD_ID
Content-Type: application/json
Authorization: USER_AUTH_TOKEN

{"album_id":"ALBUM_ID"}
```

This separate update needs the collection's Update rule to allow it.

On a duplicate, use the returned `media_id`, key, and URLs. Do not append another optimistic record or assume the requested destination key was used. The server can return an object originally stored under another filename.

Uploads predating database integration can be linked by resubmitting their files. Indexed cloud content is reused. An upload whose storage operation completed but database save failed returns HTTP 500; retrying repairs the mapping.

### Progress and retry states

Model the upload queue locally, for example:

```text
queued → sending to server → storing remotely → success / duplicate
                                      ↘ retryable error
```

XHR upload progress measures **browser-to-server bytes only**. Reaching 100% does not mean Telegram/S3 has finished. Show “Storing file…” until the final response. The current backend exposes no HTTP cloud-progress events, upload job ID, or polling endpoint.

The upload handler does not persist intermediate `pending`, `uploading`, or `error` states for new requests. Those enum values exist in the schema, but the queue must not rely on them for live progress. A collection realtime subscription is not a cloud-upload progress feed.

Retry the same file with the same key. The browser sends the full file again; acknowledged cloud chunks/parts are reused. Aborting a request does not provide a remote-delete operation. Telegram transfers are serialized, so a competing transfer may return HTTP 409; queue uploads sequentially for a predictable test experience.

## 4. Playback and downloads

Use:

```text
/api/media/{media_id}/stream
```

The server selects the backend and key from the record. Sending a backend/key directly to this route is unsupported. A successful upload's `stream_url` is equivalent to this derived URL.

### Public playback

No test token, file token, or PocketBase login is needed to view active media. Startup makes the `media_item` List and View rules public while preserving its write rules. Other collections keep their existing rules.

```ts
video.src = `${baseURL}/api/media/${encodeURIComponent(media.id)}/stream`;
video.controls = true;
video.preload = "metadata";
video.playsInline = true;
```

Use the same direct URL for images, audio, and downloads. Trashed media still returns 404. Upload requests require `X-S3-Test-Token`; metadata changes and Trash actions keep their PocketBase write authorization.

### Range behavior

Let native media elements request ranges themselves. Do not fetch the entire video into a Blob before playback; that discards the benefit of server streaming.

- Full GET: HTTP 200 with the original file's MIME type and length.
- Single byte range: HTTP 206 with `Content-Range`.
- HEAD: Metadata only; no media download.
- Invalid, unsatisfiable, or multiple ranges: HTTP 416.
- ETag, Last-Modified, and conditional requests are supported.

Telegram chunks are fetched only when they overlap the requested bytes, then streamed in order. There is no full-file merge first. Seeking within a chunk may require downloading and discarding its leading bytes. Buffering, unsupported codecs, and MP4 metadata placement still affect playback.

The response uses an inline `Content-Disposition` with the original filename. For a same-origin download action, use an anchor to the stream URL with a `download` attribute. For other deployment layouts, verify browser download behavior separately.

## 5. Existing browser test viewer

Open `/api/test/media/view` without logging in. The page lists active, completed test uploads and supports image previews, video/audio playback, posters, and downloads. Viewing sessions and cookies are no longer required.

`GET /api/test/media?offset=0` returns up to 50 test objects as `{items, next_offset}`. Items include `id`, `name`, `mime_type`, `size_bytes`, `backend`, `stream_url`, `thumbnail_url`, and `thumbs`. Follow `next_offset` until null. Test stream/thumbnail routes expose only keys under `tests/`; the ordinary `/api/media/{id}/stream` route serves all active media publicly.

## 6. Server hooks and UI consequences

| Backend behavior | Frontend consequence |
| --- | --- |
| Startup creates `unsorted`, ID `unsorted` | Offer it as the default destination; do not generate a replacement ID |
| `unsorted` cannot be renamed or deleted | Disable those actions for that album |
| Album names are trimmed and unique ignoring case | Surface validation errors; `Travel` and `travel` conflict |
| Media create/update/delete recounts affected albums transactionally | Refetch album counts after mutations; do not PATCH `media_count` |
| Missing media album is assigned to `unsorted` | Clearing `album_id` moves a record there rather than leaving it ungrouped |
| Startup repairs album assignments and counts | Refresh cached library data after server restarts |
| Storage mapping is unique by backend/bucket/key | Handle record-save validation errors; avoid manually fabricating mapping fields |
| Permanent deletion queues cloud cleanup | Show Trash separately from permanent deletion; remote cleanup is asynchronous and can fail |
| Album rename/path edits do not move cloud objects | Present them as library organization, not physical storage operations |

When moving a media record between albums, updating `album_id` is sufficient. The server adjusts both album counts. Album covers use `cover_media_id`; fetch that media record and use its public stream or thumbnail URL for an image cover where appropriate. Video posters are generated on demand through the thumbnail endpoint.

## 7. Errors and empty states

Custom routes commonly return `{ "error": "..." }`; PocketBase endpoints generally return `{ "status", "message", "data" }`. Support both shapes in a shared error adapter.

| Status / situation | Frontend behavior |
| --- | --- |
| Upload 400 | Show the invalid file-field, key, or hash message; correct the input |
| Upload route 401 | Supply the configured upload test token |
| Stream 404 | Treat as unavailable or inaccessible; the response deliberately does not distinguish every permission failure |
| Stream 409 | Show “File is not ready for playback”; its mapping/status is incomplete |
| Telegram upload 409 | Keep the queue entry and retry after the active transfer finishes |
| Upload 413 | Explain request/file size limits; avoid repeatedly sending the same oversized request |
| Stream 416 | Check a manually supplied range; native players normally manage ranges themselves |
| Upload 500 after storage succeeded | Offer retry to link the stored file to its database record |
| Storage 502 | Show a retry action and preserve the upload key/file queue entry |
| Stream 503 | Storage backend configuration is unavailable; reconnecting the user alone will not fix it |
| Video decode error | Offer downloading the original; successful streaming does not imply browser codec support |
| Empty test viewer | Only indexed, completed test records appear; unlinked Telegram messages are not a library listing |

## 8. Backend work still needed for a full frontend

These are implementation gaps, not hidden dashboard features:

- A normal-user upload endpoint authorized by PocketBase rules, with an explicit album/record contract. The current route uses one shared test token and `tests/` keys.
- Cloud-upload progress/job endpoints, a browser resume protocol, and a remote cleanup API.
- Automatic metadata extraction, useful duration values, image dimensions, and richer video metadata.
- Automatic tag counts and library statistics. Their fields exist, but the current hooks maintain **album counts only**.
- Remote object deletion and cloud moves/renames tied to library actions.
- A server-info/storage-settings API and a download/sync/migration workflow. `ServerInfo`, `SyncProgress`, `SyncStatus`, and `SavedServer` in `models.ts` do not establish implemented server endpoints.

Build the initial frontend around collection browsing, album/tag organization, public streaming, and the existing test upload/viewer flow. Keep planned features separate until their backend contracts exist.

## Implementation references

- [Upload route and response](routes/s3_test_upload.go)
- [Record adoption and storage mapping](routes/media_record.go)
- [Public streaming](routes/media_stream.go)
- [Public viewer, list, and test streaming](routes/test_media.go)
- [Working browser viewer](routes/test_media.html)
- [Collection creation and upgrades](init/base_app.go)
- [Album hooks](init/album.go)
- [Frontend model declarations](models.ts)

## On-demand thumbnails

`GET /api/media/{id}/thumb` creates a thumbnail only if it is missing, then returns `image/jpeg`. Subsequent requests serve the persisted file without contacting Telegram/S3 or running FFmpeg. Old completed uploads are supported without a bulk migration. The test viewer requests posters when a video is selected; uploads also return `thumbnail_url`, and the test listing includes `thumbnail_url` and the current `thumbs` filename.

The server requires FFmpeg (`sudo apt install ffmpeg` on Ubuntu). Generation fits the first image/video frame inside 320×320, keeps its aspect ratio, and does not upscale small files. The result is a single public PocketBase file, not a database blob. Files stay in PocketBase storage and are cleaned up by its normal record/file lifecycle. Include these files in backups.

One generation runs at a time, using one decoder/encoder thread and a 45-second deadline. An uncached concurrent request receives **503** and `Retry-After: 2`; it is not queued. A failed generation returns **503** with a five-minute retry delay, retained in memory until expiry or restart. Use a placeholder during failures; originals remain streamable. FFmpeg reads the logical object through a private temporary loopback range server, so split videos do not need to be merged or downloaded to disk in full. Depending on format and metadata layout, it may read several ranges/chunks.

Thumbnail endpoints are public: no test token, PocketBase auth, or file token is required. This applies to cached previews and first-time generation. Original streams are also public. Anyone with a media record ID can view its preview.

```ts
const response = await fetch(`${pb.baseURL}/api/media/${media.id}/thumb`);
if (response.ok) {
  const preview = URL.createObjectURL(await response.blob());
  video.poster = preview; // or image.src = preview
  // Revoke this object URL when the preview is removed.
} else if (response.status === 503) {
  const seconds = Number(response.headers.get('Retry-After') || 2);
  // Retry later while the item remains visible; keep a placeholder.
}
```

Request thumbnails for visible items, preferably with limited frontend concurrency. Do not request every item in the library on each page load. A missing thumbnail's `HEAD` request returns **404** without generating it; once saved it returns **200**. Non-image/video objects return **415** on GET, and incomplete storage mappings return **409**. Cached responses support ETag/Last-Modified revalidation.

You can also use `pb.files.getURL(media, media.thumbs)` once `thumbs` is populated; this serves the saved file and does not trigger generation. Refresh the record to obtain the generated filename. Changing the file hash, storage mapping/ETag, MIME type, or upload status clears `thumbs`; editing albums or display names keeps it. A missing saved file is regenerated on the next GET. No cron job is required.

## Telegram cleanup after deletion

Delete media through the ordinary PocketBase record API (`pb.collection('media_item').delete(id)`), subject to its Delete rule. A transactional hook records Telegram cleanup in the internal `_media_storage_deletions` table; a rolled-back deletion cannot remove Telegram files. Record deletion and thumbnail cleanup finish immediately. Remote cleanup runs after commit, one job at a time, with a one-minute polling fallback and on startup. No external cron is needed.

The worker deletes every Telegram message referenced by the logical object, including split chunks, then removes its storage/deduplication mapping. It retains chunk references when a Telegram request fails, and retries with increasing delays up to six hours. Already-deleted messages are treated as success. Objects still referenced by a live media item or shared chunk mappings are preserved. Test uploads and cleanup run under the same lifecycle lock; a changed object ETag prevents deletion of a replaced object.

A successful record DELETE does not prove remote deletion has completed. Failures appear in server logs and in `_media_storage_deletions.last_error`, with `attempts` and `next_attempt`. The queue survives restarts. Telegram's Bot API only deletes messages younger than 48 hours, and requires suitable chat permissions. Older messages may remain in Telegram; failed jobs and their chunk metadata remain available for inspection. See [Telegram deletion restrictions](https://core.telegram.org/bots/api#deletemessage). The queue supports Telegram and S3. S3 cleanup deletes the current object; a versioned bucket may retain older versions.

## Trash API and capability detection

Public `GET /api/media/capabilities` returns `{"trash":true,"retention_days":30,"cloud_deletion":true}`. No token is needed for this read-only endpoint. A 404 indicates an older backend. Capability flags describe supported routes, not a guarantee that a remote storage provider will accept every cleanup request.

| Method | Route | Behavior |
| --- | --- | --- |
| POST | `/api/media/{id}/trash` | Set `trashed_at` and `trash_expires_at` with a 30-day recovery period |
| POST | `/api/media/{id}/restore` | Restore before expiry; clear both timestamps |
| DELETE | `/api/media/{id}/permanent` | Delete an already-trashed record and queue cloud cleanup |

These routes require PocketBase auth, with the media Update rule for Trash/restore and Delete rule for permanent deletion. Success returns `{"ok":true}`. Timestamps are Unix milliseconds; zero means active. Repeating Trash does not extend its recovery period. Restoration after expiry is rejected. Filter normal library queries with `trashed_at = 0` and Trash queries with `trashed_at > 0`.

Trashed records do not count toward album totals and their original streams return 404. The cleanup worker purges expired records and queues their cloud cleanup. Re-uploading a trashed file explicitly restores its matching record. Public thumbnail URLs remain available until the record is permanently removed.
