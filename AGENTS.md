# AGENTS.md

Guidance for coding agents working in this repository. Keep it accurate as the
code changes.

## What this is

`github.com/go-faster/fs` — an S3-compatible object storage server that runs as
a single node or as a replicated, failure-domain-aware cluster. It ships as both
a CLI (`cmd/fs`) and an embeddable Go library (`server`, `engine`,
`storagemem`). SigV4 auth is on by default; responses are S3 XML. Go 1.25.

Status is **experimental**: the single-node server is mature and heavily
conformance-tested; cluster mode
([#279](https://github.com/go-faster/fs/issues/279)) is functional and still
hardening.

Read [ARCHITECTURE.md](ARCHITECTURE.md) for the layered design, package
responsibilities, request lifecycle, and extension points. The summary below
is the working checklist; ARCHITECTURE.md is the reference.

## Pre-production: break things freely

There is no released version and no deployment to be compatible with. Until
v1.0 you may change anything, and you should prefer the correct design over the
compatible one:

- **On-disk formats.** Change them. Bump the version stamp and let the next
  start discard and rebuild — that is what the stamps are for. Do not write a
  migrator, a dual-read path, or a compatibility shim.
- **Go API.** `fs.Storage`, `server.Config`, the domain types and the public
  packages may all take breaking changes. Additive-only is a v1.0 rule, not a
  rule for now.
- **Config, flags, metric and admin-API shapes.** Rename or remove them.
- **Wire behavior that is wrong.** Fix it to match AWS rather than preserving
  our own past mistake.

Prefer one clean change over a compatible one plus a cleanup that never comes.
A PR that adds a shim to avoid a rebuild is usually the wrong PR.

What this does **not** license:

- **Silent misreads.** A format change must fail loudly or rebuild — never
  interpret old bytes as new ones. Breaking is fine; corrupting is not.
- **Skipping the gates.** `make test`, the linter and the s3-tests
  known-failures list apply exactly as before. Never *add* a line to that list
  to get CI green.
- **Stale docs.** "Keeping documentation current" below still binds, and binds
  harder here: if you break something a doc describes, fix the doc in the same
  change.
- **Unannounced operator pain.** A change that forces a rebuild or a config
  edit is fine, but say so in the PR — a rolling upgrade walks every node
  through it.

This section expires at v1.0, when on-disk formats become versioned with
automated resumable migration and the public API becomes additive-only.

## Layout

- `fs.go`, `storage.go`, `errors.go` — root package: domain types
  (`Bucket`, `Object`, `*Request`/`*Response`), the `fs.Storage` interface, and
  the `Err*` sentinels. This is the API every layer speaks.
- `internal/core/handler` — HTTP/S3 wire layer (routing, XML, error mapping,
  auth/CORS middleware).
- `internal/core/service` — validation layer wrapping a backend; implements
  `fs.Storage`.
- `internal/sigv4` — SigV4 verification (header, presigned, streaming chunk
  signatures). Verified against the real aws-sdk-go-v2 signer.
- `auth`, `cors` (public) — credential/grant store and per-bucket CORS config,
  wired via `server.WithAuth` / `server.WithCORS`. `auth.Manager` is the local
  (file) credential store.
- `engine` (public, formerly `internal/engine`) — the persistent storage,
  described below; `storagemem` — the in-memory `fs.Storage` backend.
- `storagetest` — exported conformance suite; every backend (and any
  third-party one) runs `storagetest.Run(t, factory)`. `RunExcept` skips named
  cases with a reason, for a known gap — never to get CI green on a new one.
- `server` — embeddable server: `NewHandler` (bare handler) and `New`
  (turnkey server with health, timeouts, graceful shutdown). No observability
  deps — callers inject via `Config.WrapHandler`.
- `cmd/fs` — cobra CLI; wires config/flags/otel around `server`.
- `internal/cluster/layout` — the planned cluster engine's partition
  assignment (#272): partitions to ordered node slots, spread over zones then
  racks, stable across changes, balanced by capacity. Pure.
- `internal/cluster/peer` — peer membership: HMAC-authenticated peer HTTP
  (`Secret`), the adopted layout persisted under the data dir, and gossip that
  spreads the highest layout version and discovers peers. A layout change is a
  transition: the versions before it stay retained (`Layouts`) — writes go to
  every retained version's replicas, reads to the oldest's — until every node
  holding data has synced the new one (`MarkSynced`, gossiped; `Skip` releases
  a node that is gone). The engine
  replicates over it; `cmd/fs/cluster.go` starts it (config `cluster:`,
  metrics), the admin API exposes it (`/api/v1/cluster/*`), and `fs layout`
  drives it.
- `internal/cluster/table` — replicated metadata tables: CRDT rows (merge
  must be commutative, associative, idempotent) keyed by partition key + sort
  key, stored locally in bbolt, written and read at quorum over the layout's
  first three slots, with read repair. `table.Write` (`write.go`) merges rows
  of several tables in one transaction per node, each row at its own quorum;
  the engine writes a PUT's or delete's rows with it. Tombstone collection (`gc.go`) queues
  rows a table's compaction would shrink and, a day later, compacts them on
  every replica only where the row is still exactly the queued one.
- `internal/cluster/meta` — the metadata rows and their merges: buckets
  (incarnation + per-setting LWW registers), objects (version list: uploads,
  versions, delete markers, null versions; Uploading → Complete → Gone),
  block refs, multipart parts and the index of uploads in flight (both
  overwritten by `Done` once the upload finishes), and each
  table's compaction (what a tombstone reduces to). Pure; merge laws are property-tested. Its package doc states
  the design limits (per-bucket size, non-atomic conditional writes).
- `internal/cluster/block` — content-addressed blocks: a local disk store
  (SHA-256 names; a CRC-32C trailer per file checked on every local read,
  peers' blocks checked against their name; corrupt copies dropped), replicated
  put/get over the hash's partition at quorum, an in-memory resync queue, and
  GC of unreferenced blocks after a grace period. Erasure coding (`ec.go`,
  #287, Reed-Solomon via `klauspost/reedsolomon`): `PutCoded`/`GetCoded`
  store a block as K data + M parity shards, shard i on slot i of the block's
  partition (the layout must be spread for width K+M), acknowledged at K+1;
  reads join the data shards and rebuild from parity only when one is
  missing. `RepairShards` (`repair.go`, run with block anti-entropy) hands
  misplaced shards to their slot's node and rebuilds this node's missing
  shards of live blocks from K others. The engine codes a bucket's blocks of
  256 KiB and up when its `scheme` setting is `ec:K,M` (admin API
  `/api/v1/buckets/{bucket}/scheme`, CLI `fs bucket scheme`;
  `Engine.CheckLayout` refuses a layout narrower than any code a bucket has
  used).
- `engine` — the storage engine (#277): `fs.Storage` over the
  replicated tables and blocks. Buckets are incarnations keyed by ID, objects
  are version lists, data is inline (≤3 KiB) or in blocks, writes to a key are
  serialized on their coordinating node. It is the server's only persistent
  storage: `engine.Open(dir, engine.Options{...})` lays out
  `<root>/.engine/` (`meta.db`, `blocks/`, `solo/`), a single node being a
  one-node layout (`cmd/fs/engine.go`: open, legacy-layout refusal,
  `fs.engine.*` metrics);
  `engine.Run` drives anti-entropy, resync, tombstone collection and block GC;
  `Engine.RotateKeys` rewraps data keys (kept in `meta.Version.Key`) onto
  the current master key, for `fs encrypt rotate`. Versioning is a view over the
  version list (null versions while unset/suspended). SSE-S3 and SSE-C seal
  data through `internal/sse` (an SSE-C key arrives on the context,
  `fs.CustomerKeyFrom`, and is never stored); multipart parts are sealed as they arrive and not
  re-encrypted at completion. Checksums (`x-amz-checksum-*`) are of the
  plaintext; a FULL_OBJECT multipart CRC is combined from the parts' CRCs
  (`checksum.Algorithm.Combine`), not computed over the reassembled body.
  Passes `storagetest` on one node and on three; CI runs the s3-tests against
  a single engine server and a three-node cluster
  (`scripts/s3tests-cluster.sh`).
- Anti-entropy lives with what it repairs: `table.Sync` and `block.Manager.Sync`
  compare per-(partition, slot) digests with the other replicas, pull what
  differs, and hand over data of partitions the layout moved away.
- `integration` — end-to-end tests driving the server via `minio-go`.
- `internal/mock` — generated mocks (moq).

Layers flow downward only: handler → service → storage. Storage knows nothing
about HTTP or S3; don't import upward.

## Build, test, lint

- `make test` — `go test -race ./...` (the gate; run before finishing).
- `make test_fast` — quick `go test ./...`.
- `go build ./...` — build everything.
- `golangci-lint run ./...` — must be clean (config in `.golangci.yml`).
- `make generate` — regenerate mocks (moq) and `docs/CONFORMANCE.md`
  (`go:generate` on `storage.go`); run after changing the `fs.Storage`
  interface or the s3-tests known-failures list.
- `make compat` — regenerate `docs/CONFORMANCE.md` from the known-failures list alone
  (CI drift-checks it).
- `make cli-smoke` — drive a live binary with aws-cli/mc/s3cmd/rclone over
  edge-case keys (installed clients only; CI runs all four).
- `make chaos` — soak a six-node cluster of real processes (`scripts/chaos`)
  under a mixed workload while killing, freezing and re-laying-out nodes;
  fails on any read outside what the acknowledged writes allow (lost, torn or
  resurrected objects). About a minute per round; `ROUNDS=n`, `ARGS=-actions 4,5`.
- `make fuzz` — actively fuzz the wire parsers (`FUZZTIME=5m` to search
  longer); `make fuzz_selftest` checks the runner's own failure handling in
  seconds, without fuzzing.

## Conventions

- **Errors:** use `github.com/go-faster/errors`. `errors.Wrap(err, "msg")` with
  no `failed:` prefix; compare with `errors.Is`/`errors.As`, never `==`.
  `errors.Wrap(nil, ...)` returns non-nil — wrap only inside `if err != nil`.
  Cross-layer errors travel as `fs.Err*` sentinels; `internal/s3err` maps them
  to S3 error codes and HTTP status and renders the XML `<Error>` body.
- **Comments** are full sentences ending with a period.
- **Style:** Uber Go style; blank lines around blocks and before `return`.
- **Logging:** `zctx.From(ctx)` (zap). Library packages stay quiet; logging
  belongs to the binary or injected middleware.
- **Commits:** Conventional Commits (`type(scope): subject`). Split unrelated
  changes into separate commits.

## When adding a storage operation

Add it to the `fs.Storage` interface, implement it in **both** `engine` and
`storagemem`, add a `storagetest` conformance case (both backends inherit it),
then `make generate` for the mock, and wire the handler/service.

## When changing S3 wire behavior

Behavior is checked against the real ceph/s3-tests suite in CI
(`.github/workflows/s3tests.yml`, gated on
`.github/s3tests/known-failures.txt`).
Prefer exact AWS semantics (error codes, ETag formulas, listing edges).

**Shrinking the known-failures list is part of every behavior change, not a
follow-up.** Whenever you implement or fix anything an S3 client can
observe (a new operation, an error code, a validation rule, a listing
edge), delete the lines it makes pass in the same PR. CI names them for
you: the whole suite runs on every pull request, and a test listed as a
known failure that now passes fails the job. The list read inverted is the
project's compatibility statement — a change that removes no lines either
needed none (rare; say so in the PR) or isn't finished.

Never *add* lines to get CI green. A new line is a regression you are
choosing to keep, and it needs a reason beside it.

After editing the list, run `make compat` to regenerate
`docs/CONFORMANCE.md` and commit both — CI fails if the doc is stale. New
wire behavior should also be covered by an SDK integration test
(`integration/`, both minio-go and aws-sdk-go-v2) and, where a client
exercises it distinctly, the CLI smoke matrix (`scripts/cli-smoke.sh`).

## When adding functionality: observability, admin API

New functionality is not finished when it works. Two surfaces go stale
silently — nothing fails to compile, no test goes red, and the gap is found by
an operator during an incident.

Ask both questions in the **same change**, and answer them in the PR even
when the answer is "nothing":

**1. Observability.** If the change adds a background process, a queue, a
retry, a failover, an elected runner, or anything that can silently fall behind:
export what an operator needs to see it happening and see it stuck. Metrics are
registered in `cmd/fs` on the telemetry meter provider (`fs.*` names, OTel
units); logging is
`zctx.From(ctx)`, and library packages stay quiet — logging belongs to the
binary or injected middleware.

Prefer the pair that makes a ratio over the single number that needs a baseline:
"pages served" and "queries issued" together say whether a listing is fanning
out, where either alone says nothing. And export the number that goes *wrong*,
not only the one that goes up — lag, backlog depth, and how many things have no
healthy replica predict an incident; a completion counter does not.

**2. Admin API.** The spec is `_oas/admin.yml` and it is the source of truth.
Edit it, then `go generate ./...` to regenerate `adminapi` (ogen), and implement
the operation in `internal/adminhandler`. Anything an operator would otherwise
have to read a log or exec into a container to learn belongs here.

A change that adds an operator-visible capability and leaves both untouched
is not done. If a surface genuinely does not apply, say which and why in the PR
— that is a decision, and it reviews differently from an omission.

## Keeping documentation current

Treat `AGENTS.md` and `ARCHITECTURE.md` as part of the code: update them in the
**same change** that makes them stale, not later. Specifically:

- Adding/removing/renaming a package or moving a responsibility between layers
  → update the "Layout" section here and the "Packages"/diagram in
  ARCHITECTURE.md.
- Changing the `fs.Storage` interface, the layer seams, or the request routing
  → update ARCHITECTURE.md (interface seam, request lifecycle, routing list).
- Adding or changing an `fs.Err*` sentinel or its HTTP mapping → update the
  error notes in both docs.
- Changing build/test/lint entry points (`Makefile`, workflows) → update the
  "Build, test, lint" section here.
- Adding a metric or an admin-API operation → make sure the
  section above was actually followed, and that `docs/` describes the new
  surface where an operator would look for it.
- Landing an S3 wire-behavior change (e.g. moving error bodies from JSON to
  XML, or adding auth/versioning) → correct the affected description; do not
  leave a doc claiming the old behavior.

If a change makes a statement in these files wrong, the change is not done
until the statement is fixed. Keep them accurate and specific, not
aspirational — describe what the code does now.

## Do not

- Create Markdown/example files unless asked.
- Expand the S3 surface without an explicit request.
  [COMPATIBILITY.md](COMPATIBILITY.md) is the authoritative scope statement:
  what it lists as implemented is in, and everything in its "Not implemented"
  section stays a typed `NotImplemented` until someone asks for it. Some are
  planned (lifecycle transitions) and some are permanent refusals (full
  IAM/STS, the full ACL grammar with arbitrary grantees, Object Lock,
  SSE-KMS) — either way, do not implement one because it seemed missing.
- Treat auth as out of scope; it is **shipped**. Cluster mode is being
  redesigned, see [#279](https://github.com/go-faster/fs/issues/279).
