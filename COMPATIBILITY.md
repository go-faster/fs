# S3 compatibility

`go-faster/fs` implements a focused subset of the Amazon S3 API **exactly**, and
returns a proper `NotImplemented` error for everything else. The guiding
principle is that a short, honest compatibility surface beats a long,
half-working one: every operation listed as implemented behaves the way real S3
clients expect (error codes, ETag formulas, listing edges, signatures), and
anything unimplemented fails cleanly with a typed error rather than
misbehaving.

Compatibility is not self-asserted. Every change is gated in CI against the
upstream [ceph/s3-tests](https://github.com/ceph/s3-tests) conformance suite —
currently **220 tests**, run against an authenticated server — and exercised
end-to-end through real SDK clients (`aws-sdk-go-v2`, `minio-go`) and the
command-line clients `aws-cli`, `mc`, `s3cmd` and `rclone`. The machine-generated
breakdown lives in [`docs/CONFORMANCE.md`](docs/CONFORMANCE.md).

Addressing is **path-style** (`https://host/bucket/key`); the server is
single-region and ignores `LocationConstraint`.

## Implemented

| Area | Operations & behavior |
|------|-----------------------|
| **Buckets** | Create, Delete, Head, List (`ListBuckets`), GetBucketLocation. Canned `x-amz-acl` on create. |
| **Objects** | Put, Get, Head, Delete, DeleteObjects (batch, idempotent). Content served with byte-range (`206`) and conditional (`If-Match` / `If-None-Match` / `If-Modified-Since` / `If-Unmodified-Since` / `If-Range`) support. Conditional PUT (`If-Match` / `If-None-Match`, incl. atomic put-if-absent). |
| **Listing** | ListObjects **V1 and V2** with `prefix`, `delimiter`, pagination (`marker` / `continuation-token` / `start-after`), `max-keys` (clamped to 1000), `encoding-type=url`, `KeyCount`, and correct CommonPrefixes / delimiter ordering. |
| **Multipart** | Create, UploadPart, UploadPartCopy (with ranges), Complete, Abort, ListParts, ListMultipartUploads. Part validation (1–10000, strictly ascending, 5 MiB minimum except the last) with the exact S3 error codes. |
| **Copy** | CopyObject (server-side), with `x-amz-metadata-directive` and `x-amz-tagging-directive` (COPY / REPLACE). |
| **Metadata** | `Content-Type`, `Cache-Control`, `Content-Disposition`, `Content-Encoding`, and `x-amz-meta-*` user metadata — stored and round-tripped. ETag returned on PUT. |
| **Tagging** | GetObjectTagging / PutObjectTagging / DeleteObjectTagging and the `x-amz-tagging` header, with the S3 limits (≤10 tags, key ≤128, value ≤256). |
| **Access control** | Canned ACLs (`private` / `public-read` / `public-read-write`) on buckets and objects, enforced for anonymous requests. |
| **Security** | AWS Signature V4 — header auth, presigned URLs (≤7-day expiry), and streaming (`aws-chunked`) uploads with per-chunk signature verification. Native TLS with hot-reloadable certificates. Per-bucket CORS with OPTIONS preflight. |

## Not implemented

The following bucket subresources and operations return a proper
`NotImplemented` (`501`) error, so clients fail fast with a typed exception
rather than silent misbehavior:

`?accelerate`, `?acl` *(echo-only; see below)*, `?analytics`, `?cors`,
`?encryption`, `?inventory`, `?lifecycle`, `?logging`, `?metrics`,
`?notification`, `?object-lock`, `?ownershipControls`, `?policy`,
`?policyStatus`, `?publicAccessBlock`, `?replication`, `?requestPayment`,
`?tagging` (bucket-level), `?versioning`, `?website`.

The object `?acl` subresource is accepted but **echo-only** — canned ACLs are
honored via the `x-amz-acl` header (above); the full `AccessControlPolicy`
grammar with arbitrary grantees is not enforced.

## Planned (post-v1)

Each requires a design document before commitment:

- **Versioning** — the highest-demand deferred item; known-costly (version-id
  migrations, reconcilers), so it needs its own design.
- **SSE-S3** — a single server-managed key first.
- **Lifecycle expiration** — `Days` + prefix subset first, then full rules.
- **Virtual-host-style addressing** (`bucket.host`).
- **Bucket-policy subset** — only if the per-key grant model proves
  insufficient.
- **Static website hosting**, **ACME / automatic TLS**.
- **Geo-replication** — asynchronous, bucket-level replication between
  independent deployments; gated on the clustered release.

## Out of scope

Rejected with rationale, so expectations are clear:

- **Full IAM policy language & STS / OIDC / LDAP** — enterprise machinery; the
  per-credential grant model (`key → {bucket-pattern: read|write|admin}`)
  covers the self-hosted need.
- **Full ACL grammar** (arbitrary grantees, enforced `AccessControlPolicy`) —
  the canned-ACL + public-access subset is implemented; the rest is
  echo-only and ownership is not modeled.
- **Object Lock / retention / legal hold** — compliance semantics without
  certified underlying storage would be misleading.
- **SSE-C and SSE-KMS**, **replication to external S3 endpoints**,
  **analytics / inventory / accelerate / request-payment**,
  **SelectObjectContent** — outside the scope of a lean object store.

## Durability & failure model

**Atomicity.** Object writes (single PUT and multipart complete) stream to a
staging file and are renamed into place, so a **torn or partially written
object is never visible** — a crash mid-write leaves at most an orphaned
temporary file, never a corrupt object in a listing. This holds regardless of
the fsync setting and is verified by a crash-consistency test that `SIGKILL`s a
writer mid-flight.

**Durability (`fsync` policy).** Configurable via `storage.fsync`:

| Policy | Guarantee |
|--------|-----------|
| `none` | Fastest; an acknowledged write may be lost on power loss (never torn). |
| `file` *(binary default)* | Object data is flushed before the write is acknowledged. |
| `file+dir` | Data **and** the directory entry are flushed, so an acknowledged write survives a power loss. |

(Directory fsync is a no-op on Windows, where the filesystem journals directory
metadata; `file+dir` degrades to `file` there.)

**Integrity.** Every object stores a full-content checksum. A configurable
verify-on-read (`integrity.verify_on_read`) rechecks it before serving and
refuses to return corrupt bytes (HTTP 500). A background scrubber
(`integrity.scrub_interval`) periodically walks all objects, reports bit-rot
loudly, and can quarantine corrupt objects so they stop being served.

**Failure scope.** The current release is **single-node**: it protects against
process crashes and (under `file` / `file+dir`) power loss, and detects on-disk
bit-rot. It does **not** protect against loss of the underlying disk — there is
no replication yet. Multi-node replication (synchronous replicas plus a
Reed-Solomon / parity copy, with failure-domain-aware placement) is planned for
the clustered release; until then, run `go-faster/fs` on redundant storage
(RAID / replicated volume) if disk-loss tolerance is required.
