# AGENTS.md

Guidance for coding agents working in this repository. Keep it accurate as the
code changes.

## What this is

`github.com/go-faster/fs` — an S3-compatible object storage server for
development and testing. It ships as both a CLI (`cmd/fs`) and an embeddable Go
library (`server`, `storagefs`, `storagemem`). Single node, no auth, XML S3
responses. Go 1.25.

Read [ARCHITECTURE.md](ARCHITECTURE.md) for the layered design, package
responsibilities, request lifecycle, and extension points. The summary below
is the working checklist; ARCHITECTURE.md is the reference.

## Layout

- `fs.go`, `storage.go`, `errors.go` — root package: domain types
  (`Bucket`, `Object`, `*Request`/`*Response`), the `fs.Storage` interface, and
  the `Err*` sentinels. This is the API every layer speaks.
- `internal/core/handler` — HTTP/S3 wire layer (routing, XML, error mapping).
- `internal/core/service` — validation layer wrapping a backend; implements
  `fs.Storage`.
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
- `make generate` — regenerate mocks after changing the `fs.Storage`
  interface (`go:generate` on `storage.go` runs moq).

## Conventions

- **Errors:** use `github.com/go-faster/errors`. `errors.Wrap(err, "msg")` with
  no `failed:` prefix; compare with `errors.Is`/`errors.As`, never `==`.
  `errors.Wrap(nil, ...)` returns non-nil — wrap only inside `if err != nil`.
  Cross-layer errors travel as `fs.Err*` sentinels; the handler maps them to
  HTTP status in `internal/core/handler/error.go`.
- **Comments** are full sentences ending with a period.
- **Style:** Uber Go style; blank lines around blocks and before `return`.
- **Logging:** `zctx.From(ctx)` (zap). Library packages stay quiet; logging
  belongs to the binary or injected middleware.
- **Commits:** Conventional Commits (`type(scope): subject`) — commitlint
  gates CI. Split unrelated changes into separate commits.

## When adding a storage operation

Add it to the `fs.Storage` interface, implement it in **both** `storagefs` and
`storagemem`, add a `storagetest` conformance case (both backends inherit it),
then `make generate` for the mock, and wire the handler/service.

## When changing S3 wire behavior

Behavior is checked against the real ceph/s3-tests suite in CI
(`.github/workflows/s3tests.yml`, gated on `.github/s3tests/allow.txt`). If you
implement a feature that makes new tests pass, promote them into the allow-list
per `.github/s3tests/README.md`. Prefer exact AWS semantics (error codes, ETag
formulas, listing edges).

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
- Add auth, versioning, or multi-node features without an explicit request —
  they are deliberate non-goals of the current scope.
