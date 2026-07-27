# AGENTS.md

Guidance for coding agents working in this repository. Keep it accurate as the
code changes.

## What this is

`github.com/go-faster/fs` — an S3-compatible object storage server that runs as
a single node or as a replicated, failure-domain-aware cluster. It ships as both
a CLI (`cmd/fs`) and an embeddable Go library (`server`, `storagefs`,
`storagemem`, `clusterstore`). SigV4 auth is on by default; responses are S3
XML. Go 1.25.

Status is **experimental**: the single-node server is mature and heavily
conformance-tested, cluster mode (M3) is functional and still hardening.

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
  (file) credential store; `auth.Sealer` seals secrets for the cluster-wide
  credential source (`auth.source: etcd`), whose etcd persistence/watch live in
  `internal/cluster/etcd` (`auth.go`) and whose seal/unseal + admin adapter is
  `cmd/fs`'s `clusterCredentials`.
- `storagefs`, `storagemem` — filesystem and in-memory `fs.Storage` backends.
- `storagetest` — exported conformance suite; both backends and any
  third-party backend run `storagetest.Run(t, factory)`.
- `server` — embeddable server: `NewHandler` (bare handler) and `New`
  (turnkey server with health, timeouts, graceful shutdown). No observability
  deps — callers inject via `Config.WrapHandler`.
- `cmd/fs` — cobra CLI; wires config/flags/otel around `server`.
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

Add it to the `fs.Storage` interface, implement it in **both** `storagefs` and
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
  planned post-v1 (SSE-S3, lifecycle) and some are permanent refusals (full
  IAM/STS, the full ACL grammar with arbitrary grantees, Object Lock,
  SSE-C/KMS) — either way, do not implement one because it seemed missing.
- Treat auth or cluster mode as out of scope. Both are **shipped**. Earlier
  revisions of this file called them non-goals; that is no longer true.
