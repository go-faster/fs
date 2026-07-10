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

- Domain types: `Bucket`, `Object`, `PutObjectRequest`, `GetObjectResponse`,
  `MultipartUpload`, `Part`, and the multipart request/response structs.
- The `fs.Storage` interface: bucket CRUD, object put/get/delete/list, and the
  multipart operations.
- Sentinel errors (`ErrBucketNotFound`, `ErrObjectNotFound`,
  `ErrUploadNotFound`, `ErrBucketAlreadyExists`, `ErrBucketNotEmpty`,
  `ErrInvalidBucketName`, `ErrUnsupportedOperation`, `ErrPreconditionFailed`).
  These are the contract for cross-layer error signalling: backends return
  them, and the handler maps them to HTTP status codes.

### `internal/core/handler` — S3 wire layer

`handler.New(store)` returns an `http.Handler` built on a `http.ServeMux` with
a single `/` catch-all. It trims the leading slash, splits the path into
`bucket`/`key` with `strings.Cut`, and dispatches on method (and, where it
matters, query parameters):

- **root `/`** — `GET` → ListBuckets.
- **bucket** (`/{bucket}`) — `GET` → ListObjects, or ListObjectVersions when
  `?versions` is present; `PUT` → CreateBucket; `HEAD` → HeadBucket; `DELETE`
  → DeleteBucket; `POST` → multipart initiate/complete.
- **object** (`/{bucket}/{key}`) — `GET`/`HEAD` (with byte-range and
  conditional support), `PUT` (including CopyObject via `x-amz-copy-source`
  and conditional PUT), `DELETE`, `POST` (multipart part/complete/abort).

Successful responses are marshalled to S3 XML (`writeXML`). Errors go through
`renderError`, which maps the sentinel via `httpStatusFromError` to an HTTP
status. Note: the error **body** is currently JSON, which is the known
divergence from real S3 clients slated to move to the standard `<Error>` XML
document — when that changes, update this paragraph.

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

`integration` drives a running server through the real `minio-go` client
end-to-end. `internal/mock` holds the moq-generated `fs.Storage` mock used by
handler tests; regenerate with `make generate` after changing the interface.

## Request lifecycle (example: `PUT /bucket/a/b.txt`)

1. `handler` routes on path+method to `PutObject`, parsing bucket/key and any
   copy-source / conditional headers.
2. It builds a `fs.PutObjectRequest` and calls the `fs.Storage` it was given —
   in the default wiring, the `service`.
3. `service.PutObject` validates the bucket name and key, then delegates.
4. The backend writes the bytes (storagefs: create parent dirs, stream to the
   file, compute the ETag; storagemem: store in the map).
5. On error, the backend returns a sentinel; the handler maps it to a status.
   On success, the handler writes the S3 response (headers, ETag).

## Testing architecture

- **Conformance** (`storagetest`) — one suite, run by every backend.
- **Handler tests** (`internal/core/handler`) — table-driven wire behaviour
  against the mock and both backends, via `httptest`.
- **Integration** (`integration`) — real `minio-go` client against a live
  server.
- **S3 conformance CI** (`.github/workflows/s3tests.yml`) — the upstream
  ceph/s3-tests suite, gated on a curated allow-list
  (`.github/s3tests/allow.txt`). This is the objective measure of real-client
  compatibility; grow the allow-list as features land.

## Extending the system

- **New storage backend:** implement `fs.Storage`, then prove it with
  `storagetest.Run`. It drops into `server.NewHandler`/`server.New` unchanged.
- **New S3 operation:** add it to the `fs.Storage` interface, implement it in
  both backends, add a `storagetest` case, `make generate` the mock, then wire
  the handler (route + XML) and service (validation).
- **Observability/middleware:** wrap via `server.Config.WrapHandler`; never add
  such dependencies to the library core.
