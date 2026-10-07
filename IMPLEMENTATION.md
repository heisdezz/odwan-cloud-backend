# Frontend implementation guide

This document describes application behavior that is not apparent from PocketBase's collection editor: custom routes, upload semantics, streaming authentication, server hooks, and features that still need backend work. It reflects the implementation in this repository, rather than a proposed API.

Use [README.md](README.md) for running the server. Use the live dashboard for collection fields and API rules; use this guide for the frontend's behavior.

## 1. Integration boundaries

There are two API surfaces:

| Surface | Use it for |
| --- | --- |
| PocketBase collection API | Browsing records, albums, tags, editing library metadata, and authentication |
| Custom application routes | Transferring file bytes, linking uploads to records, and streaming cloud objects |

A `media_item` record has **no PocketBase file field**. Its paths and storage fields are strings pointing to an external object. Creating a record through the standard collection API does not upload bytes. `pb.files.getURL(record, filename)` and PocketBase thumbnail URLs do not apply to these media objects.

Currently, the only HTTP upload route is a **test-token-authorized** route. A normal PocketBase user's auth token does not authorize it. The ordinary streaming route does support PocketBase user permissions.

### Custom route contract

| Method | Route | Authorization | Response |
| --- | --- | --- | --- |
| GET | `/api/test/connection` | None | HTTP 200, plain text `ok` |
| POST | `/api/test/s3/upload` | `X-S3-Test-Token` | Stored object information, record ID, stream/view URLs |
| GET, HEAD | `/api/media/{id}/stream` | Collection View rule; auth header or short-lived file token where needed | Original file bytes or metadata |
| GET | `/api/test/media/view` | Public page shell; connect inside the page | Built-in browser viewer |
| POST | `/api/test/media/session` | `X-S3-Test-Token` | Viewing cookie and `expires_at` |
| DELETE | `/api/test/media/session` | Clears the viewing cookie | HTTP 204 |
| GET | `/api/test/media?offset=0` | Test header or viewing cookie | Completed test uploads and `next_offset` |
| GET, HEAD | `/api/test/media/{id}/stream` | Test header or viewing cookie | Test object's bytes or metadata |

The connection test is always registered. The other `/api/test/...` routes above are registered only when `config.json` has a nonempty `test_token`. Normal streaming remains registered when test routes are disabled. Backend selection and credentials are server configuration, not frontend request fields.

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

### Authentication

The normal stream checks `media_item`'s View rule. Records are locked by default unless rules are configured. This repository does not create a normal frontend user collection or define your user-access policy automatically.

| Credential | Where it works |
| --- | --- |
| PocketBase auth token in `Authorization` | Normal streams, subject to the View rule |
| Short-lived file token in `?token=...` | Normal streams, subject to the View rule |
| Test token in `X-S3-Test-Token` | Uploads and test routes; normal streams only for `tests/` objects |
| Test viewer cookie | `/api/test/media` routes only |

Native `<video>`, `<audio>`, and `<img>` elements cannot attach your custom auth header. For protected media, obtain a short-lived file token using the authenticated file-token endpoint:

```ts
async function protectedMediaURL(
  baseURL: string,
  authToken: string,
  mediaID: string,
) {
  const response = await fetch(new URL("/api/files/token", baseURL), {
    method: "POST",
    headers: { Authorization: authToken },
  });
  const body = await response.json();
  if (!response.ok) {
    throw new Error(body.error ?? body.message ?? "Could not authorize playback");
  }
  const url = new URL(`/api/media/${encodeURIComponent(mediaID)}/stream`, baseURL);
  url.searchParams.set("token", body.token);
  return url.toString();
}

video.src = await protectedMediaURL(baseURL, authToken, media.id);
video.controls = true;
video.preload = "metadata";
video.playsInline = true;
```

The token is still governed by the user's View rule; it does not grant access to every record. Regular auth tokens and test tokens do not work in the stream query string. Avoid storing file-token URLs as durable record data. On expiry, obtain a new token; if replacing a player's URL, retain the playback time and restore it after metadata loads where the format supports seeking.

Publicly readable media can use the normal stream URL directly. The PocketBase JavaScript client's `pb.files.getToken()` is also used by the existing [streaming guide](routes/README.md).

### Range behavior

Let native media elements request ranges themselves. Do not fetch the entire video into a Blob before playback; that discards the benefit of server streaming.

- Full GET: HTTP 200 with the original file's MIME type and length.
- Single byte range: HTTP 206 with `Content-Range`.
- HEAD: Metadata only; no media download.
- Invalid, unsatisfiable, or multiple ranges: HTTP 416.
- ETag, Last-Modified, and conditional requests are supported.

Telegram chunks are fetched only when they overlap the requested bytes, then streamed in order. There is no full-file merge first. Seeking within a chunk may require downloading and discarding its leading bytes. Buffering, unsupported codecs, and MP4 metadata placement still affect playback.

The response uses an inline `Content-Disposition` with the original filename. For a same-origin download action, use an anchor to the authorized URL with a `download` attribute. For other deployment layouts, verify browser download behavior separately.

## 5. Existing browser test viewer

The built-in page is at `/api/test/media/view`; upload responses include a `view_url` selecting the returned record.

For a same-origin test frontend, connect without putting the test token into a media URL:

```ts
await fetch("/api/test/media/session", {
  method: "POST",
  credentials: "same-origin",
  headers: { "X-S3-Test-Token": enteredToken },
});

const response = await fetch("/api/test/media?offset=0", {
  credentials: "same-origin",
});
const page = await response.json();
```

Check `response.ok` on both requests before proceeding. The session response contains `expires_at`; its HttpOnly cookie lasts one hour and cannot be read by JavaScript.

The list response is:

```ts
interface TestMediaPage {
  items: Array<{
    id: string;
    name: string;
    mime_type: string;
    size_bytes: number;
    backend: "telegram" | "s3";
    stream_url: string; // /api/test/media/{id}/stream
  }>;
  next_offset: number | null;
}
```

This endpoint returns at most 50 records, ordered by `created_at DESC, id DESC`. It only includes completed records with storage mappings and keys starting with `tests/`. Follow `next_offset` until it is `null`; a full last page can lead to one empty follow-up page.

Use each item's **test** `stream_url` with native media elements. The viewing cookie does not authorize the normal `/api/media/...` URL. DELETE `/api/test/media/session` clears the cookie. A 401 during listing or playback means reconnect.

This cookie is SameSite and path-scoped. The shipped viewer is designed for the same origin as PocketBase. A frontend hosted on another site must use a same-origin proxy or normal PocketBase/file-token authentication for playback; `credentials: "include"` alone does not bypass SameSite restrictions.

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
| Deleting a media record does not delete Telegram/S3 objects | “Delete from library” is accurate; “Delete cloud file permanently” is not implemented |
| Album rename/path edits do not move cloud objects | Present them as library organization, not physical storage operations |

When moving a media record between albums, updating `album_id` is sufficient. The server adjusts both album counts. Album covers use `cover_media_id`; fetch that media record and use its authorized stream URL for an image cover where appropriate. There is no automatic video-poster generation.

## 7. Errors and empty states

Custom routes commonly return `{ "error": "..." }`; PocketBase endpoints generally return `{ "status", "message", "data" }`. Support both shapes in a shared error adapter.

| Status / situation | Frontend behavior |
| --- | --- |
| Upload 400 | Show the invalid file-field, key, or hash message; correct the input |
| Test route 401 | Ask for the test token again or reconnect the viewing session |
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
- Automatic metadata extraction, useful duration values, image dimensions, and video thumbnails/posters.
- Automatic tag counts and library statistics. Their fields exist, but the current hooks maintain **album counts only**.
- Remote object deletion and cloud moves/renames tied to library actions.
- A server-info/storage-settings API and a download/sync/migration workflow. `ServerInfo`, `SyncProgress`, `SyncStatus`, and `SavedServer` in `models.ts` do not establish implemented server endpoints.

Build the initial frontend around collection browsing, album/tag organization, authorized streaming, and the existing test upload/viewer flow. Keep planned features separate until their backend contracts exist.

## Implementation references

- [Upload route and response](routes/s3_test_upload.go)
- [Record adoption and storage mapping](routes/media_record.go)
- [Streaming and access checks](routes/media_stream.go)
- [Viewer session, list, and test streaming](routes/test_media.go)
- [Working browser viewer](routes/test_media.html)
- [Collection creation and upgrades](init/base_app.go)
- [Album hooks](init/album.go)
- [Frontend model declarations](models.ts)
