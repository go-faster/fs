# Architecture

How `go-faster/fs` is put together. This describes the current code, not
aspirations; keep it in sync when the structure changes (see
[AGENTS.md](AGENTS.md) → "Keeping documentation current").

## Purpose and scope

A single-node S3-compatible object storage server returning S3 XML responses.
It is usable two ways:

- as a **CLI** (`cmd/fs`) — a turnkey server with health checks, timeouts,
  graceful shutdown and OpenTelemetry wiring;
- as an **embeddable library** — mount the S3 handler into your own server, or
  run the managed `server.Server`, with a pluggable storage backend.

A Garage-style cluster (zone/rack-aware replication) is being built, see
[#279](https://github.com/go-faster/fs/issues/279). Its first piece is
`internal/cluster/layout`, the pure partition-to-node assignment, and
`internal/cluster/peer`, which authenticates peer traffic and gossips the
layout between nodes. `cmd/fs` runs membership when `cluster.node_id` is
set, and the admin API applies layouts. `internal/cluster/table` adds
replicated CRDT tables on top, and `internal/cluster/meta` the bucket, object
(version list) and block-ref rows they hold, and `internal/cluster/block`
the content-addressed data blocks. The public `engine` package implements
`fs.Storage` over them and passes the conformance suite on one node and on
three; it is the server's only persistent storage, a single node being a
one-node layout.

Scope is stated by [COMPATIBILITY.md](COMPATIBILITY.md), not here: what it
lists as implemented is in, and everything in its "Not implemented" section
returns a typed `NotImplemented`.

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
   │  storage backend  │  engine / storagemem / your own
   │  (bytes)          │  no knowledge of HTTP or S3
   └───────────────────┘
```

The seam between every layer is the **`fs.Storage`** interface
(`storage.go`). The service is a validating decorator that implements
`fs.Storage` and wraps a backend; the handler is constructed over an
`fs.Storage` and does not know whether validation or a raw backend sits behind
it. This is why a custom backend, the in-memory backend, and the engine are
all interchangeable.

## Packages

### Root package (`fs.go`, `storage.go`, `errors.go`)

The shared vocabulary every layer speaks:

- Domain types: `Bucket`, `Object`, `PutObjectRequest`/`PutObjectResponse`
  (the response carries the stored ETag), `GetObjectResponse`,
  `ListObjectsRequest`/`ListObjectsResponse` (one page of a listing: prefix,
  delimiter, start-after and limit in, folded entries and truncation out),
  `ObjectMetadata` (representation headers + `x-amz-meta-*` pairs), `Tag`,
  `MultipartUpload`, `Part`, and the multipart request/response structs
  (`CreateMultipartUploadRequest` carries metadata/tags/ACL/owner applied at
  completion), `Owner` (the principal recorded on an object), `ACL`.
- The `fs.Storage` interface: bucket CRUD, object put/get/delete and paged
  listing (`ListObjectsRequest.FoldPage` is the shared folding and paging
  rule every backend applies, so common prefixes count toward the limit the
  same way everywhere),
  object tagging (get/put/delete), canned ACLs (`SetBucketACL`/`BucketACL`,
  `SetObjectACL`/`ObjectACL`), object ownership (`ObjectOwner`), and the
  multipart operations (including `ListParts`/`ListMultipartUploads`).
- Sentinel errors (`ErrBucketNotFound`, `ErrObjectNotFound`,
  `ErrUploadNotFound`, `ErrBucketAlreadyExists`, `ErrBucketNotEmpty`,
  `ErrInvalidBucketName`, `ErrInvalidKey`, `ErrUnsupportedOperation`,
  `ErrPreconditionFailed`,
  `ErrInvalidPart`, `ErrInvalidPartOrder`, `ErrInvalidPartNumber`,
  `ErrEntityTooSmall`, `ErrInvalidTag`, `ErrCustomerKeyMismatch` — an SSE-C
  key missing for an object encrypted with one, or sent for one that is not,
  mapped to 400 InvalidRequest; a wrong key is `ErrAccessDenied`, 403).
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
  support; `?tagging` → GetObjectTagging, `?acl` → GetObjectACL, `?uploadId`
  → ListParts), `PUT` (CopyObject via `x-amz-copy-source` with metadata/tagging
  directives, UploadPart/UploadPartCopy via `?partNumber&uploadId`,
  `?tagging` → PutObjectTagging, `?acl` → PutObjectACL, conditional PUT),
  `DELETE` (`?tagging` →
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
handler's `Authenticator` interface (`Secret`, `Allow`, `PublicRead`, `Owner`).
`Owner` resolves an access key to the `auth.Identity` (`user_id` /
`display_name`, defaulting to the access key) that objects it writes are owned
by.

Credentials are config/env keys plus runtime keys the admin API creates, held
by `auth.Manager` and persisted to a local JSON file.

Anonymous (unsigned) requests are authorized against **canned ACLs**
(`private` / `public-read` / `public-read-write`) stored per bucket and per
object: the auth middleware consults `fs.Storage.BucketACL`/`ObjectACL`
directly. Reads need a public-read bucket or object; writes need a
public-read-write bucket; bucket create/delete are never anonymous. A missing
bucket/object is let through so the router returns the natural 404
(existence-first ordering, matching RGW) rather than a blanket 403.

The object `?acl` subresource is served from that same canned level:
`GetObjectACL` renders it as the grants it implies (the owner's `FULL_CONTROL`,
plus `AllUsers` `READ`/`WRITE` for the public levels) and `PutObjectACL` reduces
a canned header or an `AccessControlPolicy` body back to one level via
`fs.Storage.SetObjectACL`. This is the canned subset only — enforcing the full
ACL grammar with arbitrary grantees is out of scope, so grants naming specific
users are accepted and ignored.

Every object records the **owner** that wrote it (`fs.Owner`, from the
authenticated credential's `auth.Identity`; `anonymous` when unsigned). It is
stored alongside the object and reported by `fs.Storage.ObjectOwner` and in
listing `<Owner>` elements — deriving it from the caller instead would make an
object appear to change hands depending on who read it.

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

- **`engine`** — the persistent storage, single node or cluster. Opened with
  `engine.Open(dir, engine.Options{Keyring, NoSync, Member})` and released
  with `Close`. Besides `fs.Storage` it implements versioning, SSE-S3,
  checksums, object attributes, ownership, bucket CORS/lifecycle/settings and
  bucket encryption. A nil `Member` runs a single node over a private one-node
  layout; in cluster mode each object's metadata and data live on three nodes
  of the layout (see [Engine](#engine)).
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
root, opens the `engine` (refusing a data directory left by the removed
filesystem backend), wraps the handler with OpenTelemetry
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
4. The backend writes the bytes (engine: store the content as blocks, or
   inline when small, then commit the version's metadata row; storagemem:
   store in the map) and returns the ETag.
5. On error, the backend returns a sentinel; the handler maps it to a status.
   On success, the handler writes the S3 response (headers, ETag).

### Engine

On disk, single node and cluster alike, the engine lives under
`<storage.root>/.engine/`:

- `meta.db` — the bbolt metadata tables (buckets, object version lists, block
  references). Every metadata write is a bbolt transaction.
- `blocks/` — content-addressed data blocks named by their SHA-256, each file
  the block plus a CRC-32C trailer (stamped by `blocks/FORMAT`; a store with
  blocks but no stamp is refused). Every local read checks the CRC, and a
  block fetched from a peer is checked against its name; a corrupt copy is
  dropped and fetched again from a replica; with no good copy reachable the
  read fails rather than serving corrupt bytes. SHA-256 is computed once per
  block on write, and again only where a peer receives it.
- `solo/` — a single node's private one-node layout.

An object of at most 3 KiB is stored inline in its metadata row; a larger one
is split into 1 MiB blocks. A block's reference row is written before the
block itself, and the block file is written to a temp file, fsynced, renamed
into place, and its directory fsynced. A version becomes current only when its
metadata row is written, after all of its blocks, so a crash never exposes a
torn object. `storage.fsync: file` (the default) fsyncs data and metadata
before a write is acknowledged; `none` (`engine.Options.NoSync`) skips fsync
and can lose acknowledged writes in a crash.

In cluster mode writes and reads use a quorum of 2 of 3 replicas; anti-entropy
repairs replicas that missed a write, and a block GC removes blocks nothing
references. There is no background scrubber: verification happens on every
read, and repair is anti-entropy's job.

**SSE-C.** A customer key never reaches storage: the handler checks its
headers (algorithm, base64 key, key MD5; over TLS or a proxy saying
`X-Forwarded-Proto: https`, unless `encryption.customer_keys_over_http`) and
puts it on the request context (`fs.WithCustomerKey`), which a copy swaps for
the copy source's key to read the source. The engine treats it exactly like a
master key — it seals a fresh per-object data key — and stores only that
sealed key and the customer key's one-way ID, so a wrong key is told apart
(403) without decrypting. An SSE-C object's ETag is the plaintext MD5 keyed by
its data key, so listing reveals nothing about the content.

**Encryption keys.** An encrypted version's data key, sealed by the master
key ring, is not in the version's write-once payload but in its own register
(`meta.Version.Key`), merged apart from the rest like `Attrs`. That is what
lets `Engine.RotateKeys` (admin `POST /api/v1/encryption/rotate`, CLI
`fs encrypt rotate`) rewrap every key under the current master key with a
newer write; an upload rotated in flight keeps the rewrapped key when it
completes, since completion carries the version's registers.

**Tombstones.** A delete is a merge like any other write: a deleted or aborted
version stays in its object row as `Gone`, a released block reference stays
`Deleted`, a finished upload's parts are overwritten by a final "done"
register — so a replica that missed the delete cannot bring the data back.
Each table queues the rows that hold one (`<table>.gc` in `meta.db`, with the
hash of the row's bytes). A day later (`engine.RunConfig.Tombstones`), the
hourly collection writes each queued row to every replica, then has every
replica drop the tombstones if its row is still exactly that; a replica that
merged a newer write meanwhile keeps it, and anti-entropy restores it to the
rest. With any replica unreachable the row waits for the next pass. The day is
what covers a write still in flight and a partition handover a layout change
started: either could carry the row from before its delete. Single nodes
collect the same way, with one replica.

**Periodic-pass scheduling.** The lifecycle sweep records when it last
completed — `<root>/.lastrun/<task>.json` — and schedules the next pass one interval
after that rather than one interval after process start. The engine's hourly
collection (tombstones, then blocks) does the same, recording in `meta.db`. Without the record a
periodic loop has to pick between two wrong answers: a ticker never fires on a
node restarted more often than the interval (redeploy hourly, never sweep), and
running on start makes a node that restarts often re-walk everything every time.
A pass is recorded only once it finishes, so an interrupted one is still due,
and a short floor keeps a crashlooping node from repeating an overdue pass on
every restart.

## Testing architecture

- **Conformance** (`storagetest`) — one suite, run by every backend.
- **Handler tests** (`internal/core/handler`) — table-driven wire behaviour
  against the mock and both backends, via `httptest`.
- **Integration** (`integration`) — real SDK clients (`minio-go` and
  `aws-sdk-go-v2`) against an in-process server, exercising each SDK's own
  request encoding (path-style addressing, checksum trailers, error typing).
- **S3 conformance CI** (`.github/workflows/s3tests.yml`) — the upstream
  ceph/s3-tests suite, run in full and gated on a deny-list
  (`.github/s3tests/known-failures.txt`) that may only shrink. This is the
  objective measure of real-client compatibility; delete lines as features
  land.
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
