# Architecture

How `go-faster/fs` is put together. This describes the current code, not
aspirations; keep it in sync when the structure changes (see
[AGENTS.md](AGENTS.md) → "Keeping documentation current").

## Purpose and scope

An S3-compatible object storage server for development and testing. It runs as
a single node, without authentication, and returns S3 XML responses. It is
usable two ways:

- as a **CLI** (`cmd/fs`) — a turnkey server with health checks, timeouts,
  graceful shutdown and OpenTelemetry wiring;
- as an **embeddable library** — mount the S3 handler into your own server, or
  run the managed `server.Server`, with a pluggable storage backend.

Deliberate non-goals in the current scope: authentication, object versioning,
and multi-node/replicated operation.

## Layered design

Requests flow strictly downward through three layers, each defined against the
domain types in the root package. Nothing in a lower layer imports a higher
one.

```
        HTTP request
             │
   ┌─────────▼─────────┐
   │  handler          │  internal/core/handler
   │  (S3 wire: route, │  parse path/query, decode/encode XML,
   │   XML, errors)    │  map fs.Err* → HTTP status
   └─────────┬─────────┘
             │ fs.Storage
   ┌─────────▼─────────┐
   │  service          │  internal/core/service
   │  (validation)     │  validate bucket/key/prefix, then delegate
   └─────────┬─────────┘
             │ fs.Storage
   ┌─────────▼─────────┐
   │  storage backend  │  storagefs / storagemem / your own
   │  (bytes)          │  no knowledge of HTTP or S3
   └───────────────────┘
```

The seam between every layer is the **`fs.Storage`** interface
(`storage.go`). The service is a validating decorator that implements
`fs.Storage` and wraps a backend; the handler is constructed over an
`fs.Storage` and does not know whether validation or a raw backend sits behind
it. This is why a custom backend, the in-memory backend, and the filesystem
backend are all interchangeable.

## Packages

### Root package (`fs.go`, `storage.go`, `errors.go`)

The shared vocabulary every layer speaks:

- Domain types: `Bucket`, `Object`, `PutObjectRequest`/`PutObjectResponse`
  (the response carries the stored ETag), `GetObjectResponse`,
  `ObjectMetadata` (representation headers + `x-amz-meta-*` pairs), `Tag`,
  `MultipartUpload`, `Part`, and the multipart request/response structs
  (`CreateMultipartUploadRequest` carries metadata/tags applied at
  completion).
- The `fs.Storage` interface: bucket CRUD, object put/get/delete/list,
  object tagging (get/put/delete), and the multipart operations (including
  `ListParts`/`ListMultipartUploads`).
- Sentinel errors (`ErrBucketNotFound`, `ErrObjectNotFound`,
  `ErrUploadNotFound`, `ErrBucketAlreadyExists`, `ErrBucketNotEmpty`,
  `ErrInvalidBucketName`, `ErrUnsupportedOperation`, `ErrPreconditionFailed`,
  `ErrInvalidPart`, `ErrInvalidPartOrder`, `ErrInvalidPartNumber`,
  `ErrEntityTooSmall`, `ErrInvalidTag`).
  These are the contract for cross-layer error signalling: backends return
  them, and `internal/s3err` maps them to S3 error codes and HTTP status.

### `internal/core/handler` — S3 wire layer

`handler.New(store)` returns an `http.Handler` built on a `http.ServeMux` with
a single `/` catch-all. It trims the leading slash, splits the path into
`bucket`/`key` with `strings.Cut`, and dispatches on method (and, where it
matters, query parameters):

- **root `/`** — `GET` → ListBuckets.
- **bucket** (`/{bucket}`) — `GET` → ListObjectsV1/V2 (split on
  `list-type=2`), ListObjectVersions on `?versions`, ListMultipartUploads on
  `?uploads`; `PUT` → CreateBucket; `HEAD` → HeadBucket; `DELETE`
  → DeleteBucket; `POST` → DeleteObjects (`?delete`).
- **object** (`/{bucket}/{key}`) — `GET`/`HEAD` (byte-range and conditional
  support; `?tagging` → GetObjectTagging, `?uploadId` → ListParts),
  `PUT` (CopyObject via `x-amz-copy-source` with metadata/tagging
  directives, UploadPart/UploadPartCopy via `?partNumber&uploadId`,
  `?tagging` → PutObjectTagging, conditional PUT), `DELETE` (`?tagging` →
  DeleteObjectTagging, `?uploadId` → AbortMultipartUpload), `POST`
  (multipart initiate/complete).

Successful responses are marshalled to S3 XML (`writeXML`). Errors go through
`renderError`/`renderAPIError`, which delegate to the `internal/s3err` package:
it holds the S3 error-code table (`APIError` = wire code + HTTP status +
message), maps the `fs.Err*` sentinels to codes, and writes the standard
`<Error><Code><Message><Resource><RequestId></Error>` XML document (no body for
HEAD; non-panicking fallback if encoding fails).

`handler.New(store, opts...)` composes middleware around the router, outermost
first: **request-id → CORS → auth → router**. So every response (including
errors) carries an `x-amz-request-id`, CORS preflight is answered before auth
can reject it, and only authenticated (or public-read) requests reach the
router. Auth and CORS are opt-in via `WithAuthenticator` / `WithCORS`; without
them the handler serves anonymously (the library default).

### `internal/sigv4` — SigV4 verification

Verifies AWS Signature Version 4 on incoming requests: Authorization-header
auth, presigned-URL (query) auth, and the seed + per-chunk signatures for
streaming (aws-chunked) uploads. It recomputes the signature from a looked-up
secret and compares in constant time — it never signs. The canonical URI is the
request's `EscapedPath()` (S3 signs with `DisableURIPathEscaping`, so no
re-encode). `ChunkVerifyingReader` decodes and verifies signed streaming chunks
as the body is read, so a tampered payload surfaces as a read error before it
reaches storage. Verified against the real aws-sdk-go-v2 signer in unit tests.

### `auth` (public) — credentials & authorization

A table, not a policy engine: `Config` → `Store` maps each access key to a
secret and a set of (bucket-glob → permission) grants (Read ⊆ Write ⊆ Admin),
with optional public-read buckets. The snapshot sits behind an atomic pointer,
so `Set` hot-reloads credentials without locking readers. `Store` satisfies the
handler's `Authenticator` interface (`Secret`, `Allow`, `PublicRead`).

Anonymous (unsigned) requests are authorized against **canned ACLs**
(`private` / `public-read` / `public-read-write`) stored per bucket and per
object: the auth middleware consults `fs.Storage.BucketACL`/`ObjectACL`
directly. Reads need a public-read bucket or object; writes need a
public-read-write bucket; bucket create/delete are never anonymous. A missing
bucket/object is let through so the router returns the natural 404
(existence-first ordering, matching RGW) rather than a blanket 403. This is the
canned subset only — full ACL grammar / `AccessControlPolicy` enforcement is
out of scope (the `?acl` subresource is echo-only).

### `cors` (public) — per-bucket CORS

`Config` holds per-bucket (and default) `Rule`s; the handler's CORS middleware
answers OPTIONS preflight and adds CORS response headers to matching
cross-origin requests. Configured at construction, not via the S3 PutBucketCors
subresource.

### `internal/s3err` — S3 error rendering

The S3 error-code table and XML `<Error>` writer. `APIError` bundles a stable
wire code, HTTP status, and default message; `FromError` resolves the `fs.Err*`
sentinels; `Write`/`WriteAPI` emit the response (skipping the body for HEAD).
This is the single place that owns the error wire format.

### `internal/core/service` — validation layer

`service.New(store)` wraps a backend and implements `fs.Storage`. Each method
validates its inputs with `internal/validate` (bucket names, object keys,
listing prefixes — including path-traversal protection) before delegating.
Validation failures surface as wrapped errors; the backend is only reached with
already-sanitised inputs.

### Storage backends

Both implement `fs.Storage` and are verified by the same conformance suite.

- **`storagefs`** — filesystem backend. Root directory contains one
  subdirectory per bucket; an object with key `a/b/c.txt` is stored at
  `<root>/<bucket>/a/b/c.txt` (`toOSPath` maps `/` to the OS separator).
  Deleting an object prunes now-empty parent directories up to the bucket
  root, so a bucket whose objects are all gone is genuinely empty and can be
  removed. ETags are MD5 digests. Multipart uploads are staged by a dedicated
  manager and assembled on completion.
- **`storagemem`** — in-memory backend backed by maps under a mutex. Returns a
  seekable reader from GetObject so the handler's range/conditional logic
  works. Intended for tests and ephemeral use.

### `storagetest` — conformance suite

`storagetest.Run(t, factory)` exercises the full `fs.Storage` contract
(bucket lifecycle, object round-trips, listing, multipart, sentinel-error
behaviour, empty-after-nested-delete, and more). Every backend — and any
third-party backend — runs it, so behavioural parity is enforced by tests
rather than convention. Add a case here when you add or change a storage
operation; both backends inherit it.

### `server` — embeddable entry points

- `server.NewHandler(store)` — the bare S3 `http.Handler` (validation +
  routing), to mount into an existing mux/server, optionally under a prefix.
- `server.New(cfg)` — a managed `Server`: health endpoint, `http.Server`
  timeouts, optional bucket pre-creation, graceful context-driven shutdown.
- `Config.WrapHandler` — the single injection point for observability and
  middleware (e.g. `otelhttp`). The library core pulls in **no** observability
  stack; that dependency lives in the caller (or in `cmd/fs`).

### `cmd/fs` — CLI

A cobra command (`fs s3`) that loads YAML/flag configuration, resolves storage
root, constructs a `storagefs` backend, wraps the handler with OpenTelemetry
and request logging, and runs `server.Server`. Server defaults are derived from
the `server` package constants so the two cannot drift.

### `integration` and `internal/mock`

`integration` drives an in-process server through the real `minio-go` and
`aws-sdk-go-v2` clients end-to-end. `internal/mock` holds the moq-generated
`fs.Storage` mock used by handler tests; regenerate with `make generate` after
changing the interface.

## Request lifecycle (example: `PUT /bucket/a/b.txt`)

1. `handler` routes on path+method to `PutObject`, parsing bucket/key and any
   copy-source / conditional headers.
2. It builds a `fs.PutObjectRequest` and calls the `fs.Storage` it was given —
   in the default wiring, the `service`.
3. `service.PutObject` validates the bucket name and key, then delegates.
4. The backend writes the bytes (storagefs: stream to a staging temp file while
   hashing, fsync per policy, rename into the bucket, then write the metadata
   sidecar; storagemem: store in the map) and returns the ETag.
5. On error, the backend returns a sentinel; the handler maps it to a status.
   On success, the handler writes the S3 response (headers, ETag).

### storagefs durability

Every object write (single PUT and multipart complete) streams to a temp file
under a root-level staging directory (`<root>/.tmp`), then renames it into the
bucket. The rename is atomic and the staging dir is outside the bucket tree, so
a crash mid-write never leaves a torn or spurious object visible to
`ListObjects` — only an orphaned temp file. The `SyncPolicy`
(`none | file | file+dir`, binary default `file`) controls durability on top of
that atomicity: `file` fsyncs object data before the rename, `file+dir` also
fsyncs the parent directory afterward so the rename survives a power loss. A
subprocess crash-consistency test (`SIGKILL` mid-write) asserts the no-torn
invariant. Sidecar and bucket-meta writes go through the same
`atomicWrite` (temp + fsync + rename).

### storagefs metadata sidecars

Object metadata (ETag, representation headers, `x-amz-meta-*`, tags) lives in
JSON sidecars under `<root>/.meta/<bucket>/<sha256(key)>.json`, outside the
bucket directories so sidecars can never collide with object keys. The
documents carry a format version stamp. A missing or corrupt sidecar degrades
gracefully: the object stays readable with default metadata and the ETag is
recomputed (and cached) on read, which keeps pre-sidecar data directories
working. Root-level dot-directories (`.meta`, `.multipart`) are internal and
never listed as buckets.

## Testing architecture

- **Conformance** (`storagetest`) — one suite, run by every backend.
- **Handler tests** (`internal/core/handler`) — table-driven wire behaviour
  against the mock and both backends, via `httptest`.
- **Integration** (`integration`) — real SDK clients (`minio-go` and
  `aws-sdk-go-v2`) against an in-process server, exercising each SDK's own
  request encoding (path-style addressing, checksum trailers, error typing).
- **S3 conformance CI** (`.github/workflows/s3tests.yml`) — the upstream
  ceph/s3-tests suite, gated on a curated allow-list
  (`.github/s3tests/allow.txt`). This is the objective measure of real-client
  compatibility; grow the allow-list as features land.
  `docs/CONFORMANCE.md` is generated from it (`make compat`, drift-checked).
- **CLI smoke matrix** (`.github/workflows/cli-smoke.yml`,
  `scripts/cli-smoke.sh`) — a live binary driven by aws-cli, MinIO `mc`,
  `s3cmd`, and `rclone` through a round-trip over edge-case object keys.

## Extending the system

- **New storage backend:** implement `fs.Storage`, then prove it with
  `storagetest.Run`. It drops into `server.NewHandler`/`server.New` unchanged.
- **New S3 operation:** add it to the `fs.Storage` interface, implement it in
  both backends, add a `storagetest` case, `make generate` the mock, then wire
  the handler (route + XML) and service (validation).
- **Observability/middleware:** wrap via `server.Config.WrapHandler`; never add
  such dependencies to the library core.
