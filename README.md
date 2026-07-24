# fs [![Go Reference](https://img.shields.io/badge/go-pkg-00ADD8)](https://pkg.go.dev/github.com/go-faster/fs#section-documentation) [![codecov](https://img.shields.io/codecov/c/github/go-faster/fs?label=cover)](https://codecov.io/gh/go-faster/fs) [![experimental](https://img.shields.io/badge/-experimental-blueviolet)](https://go-faster.org/docs/projects/status#experimental)

S3-compatible object storage that scales from a single binary to a replicated
cluster. It began as a lightweight server for development and testing, and now
also runs as a distributed, failure-domain-aware object store — while staying a
single static binary and an embeddable Go library.

> **Status: experimental.** The single-node server is mature and heavily
> conformance-tested; the distributed cluster mode (M3) is functional but still
> hardening. Pin a version and read [COMPATIBILITY.md](COMPATIBILITY.md) and
> [docs/FAILURE-MODEL.md](docs/FAILURE-MODEL.md) before trusting production data
> to it.

## Features

### Core S3 server

- Bucket operations (create, delete, list) and object operations (put, get,
  delete, list, copy, tagging, metadata, `x-amz-meta-*`).
- Multipart uploads, presigned URLs (≤7-day expiry) and streaming (chunked)
  uploads.
- **AWS Signature V4** auth by default: multiple credentials, per-bucket grants
  (`read`/`write`/`admin`), public-read buckets and canned ACLs.
- Hot-reloadable TLS; credential and certificate reload on `SIGHUP` with no
  restart.
- Crash-atomic writes, `fsync` policy control, a background bit-rot scrubber and
  optional verify-on-read.
- Compatible with the AWS CLI, MinIO client (`mc`), `s3cmd`, `rclone` and the
  AWS SDKs; liveness/readiness endpoints and OpenTelemetry metrics/traces.

### Distributed cluster mode (M3)

- Objects placed across **failure domains** (rack → node → disk), written at
  **quorum**, and served by **any node** (transparent proxying).
- Pluggable **replication schemes**: `rf2.5` (2 replicas + half-parity), `rf3`
  (3 replicas), or **Reed-Solomon erasure coding** `ec:k,m` (e.g. `ec:4,2` at
  1.5× overhead) — configurable per cluster and **per bucket**.
- **etcd control plane** for membership, topology, epochs and cluster config;
  online **rebalancing** and background **repair/scrub** converge placement
  after membership changes with no operator action.
- Headless **`fs admin`** control-plane process: cluster-wide status dashboard
  and rebalance control without being a data node.
- **Cluster-wide runtime key management** (`auth.source: etcd`): credentials
  live in etcd with secrets sealed by an HKDF key derived from the cluster
  secret, and every node hot-reloads add/rotate/delete with no restart.

### Operability

- Admin API + web dashboard on a separate bearer-token listener (runtime
  access-key CRUD, cluster status, rebalance control, per-bucket schemes).
- `systemd` unit generation (`fs systemd`), commented config generation, and a
  library core with **no forced observability stack**.

**Quick Start:**
```bash
# Install
go install github.com/go-faster/fs/cmd/fs@latest

# Start the server
fs s3

# Or with custom configuration
fs s3 --addr :9000 --root /data/s3
```

See [COMPATIBILITY.md](COMPATIBILITY.md) for the full compatibility statement
(what's implemented, what returns `NotImplemented`, what's planned, and the
durability & failure model). Compatibility is measured against the upstream
[ceph/s3-tests](https://github.com/ceph/s3-tests) suite and real S3 clients —
the machine-generated breakdown is in the
[S3 conformance report](docs/CONFORMANCE.md).

**Example Usage:**
```bash
# Using AWS CLI
export AWS_ENDPOINT_URL=http://localhost:8080
aws s3 mb s3://mybucket --endpoint-url $AWS_ENDPOINT_URL
aws s3 cp file.txt s3://mybucket/ --endpoint-url $AWS_ENDPOINT_URL

# Using cURL
curl -X PUT http://localhost:8080/mybucket
curl -X PUT -d "Hello!" http://localhost:8080/mybucket/hello.txt
curl http://localhost:8080/mybucket/hello.txt
```

## Authentication & TLS

The binary authenticates requests with **AWS Signature V4 by default**. Provide
a root credential and (optionally) TLS:

```bash
export FS_ROOT_ACCESS_KEY=AKIAEXAMPLE
export FS_ROOT_SECRET_KEY=exampleSecretKey
fs s3 --tls-cert cert.pem --tls-key key.pem
```

Additional keys, per-bucket grants (`read`/`write`/`admin`), and public-read
buckets are configured under `auth:` in the config file. To run without any
authentication (development only), pass `--insecure-no-auth`.

SigV4 header auth, presigned URLs (≤7-day expiry) and streaming uploads are all
verified; TLS certificates hot-reload without dropping connections. As a
library, enable it with `server.WithAuth(store)` / `server.WithCORS(cfg)` — the
bare handler stays anonymous unless you opt in.

### Admin API & access-key dashboard

Multiple access-key/secret credentials can be managed **at runtime** — without a
restart — through a separate admin listener that also serves a small web
dashboard. In single-node (`auth.source: file`) mode, config-defined keys stay
read-only and keys created through the admin API are persisted
(`<root>/.access-keys.json`, mode `0600`) and survive restarts and `SIGHUP`
reloads. In cluster mode with `auth.source: etcd`, the same endpoints manage the
**cluster-wide** credential store, and changes propagate to every node — and to
the headless `fs admin` — with no restart.

```yaml
admin:
  enabled: true
  addr: "localhost:8090"   # keep bound to localhost or behind a proxy
  token: "change-me"       # or set FS_ADMIN_TOKEN
```

The dashboard (open `http://localhost:8090/`, paste the token) lists credentials
and their grants, creates keys (generating the access key and secret, shown
once), and deletes runtime keys. The same operations are available as a JSON API
under `/api/v1` (bearer-token protected), generated from
[`_oas/admin.yml`](_oas/admin.yml) with [ogen](https://github.com/ogen-go/ogen);
the dashboard is a TypeScript/React SPA whose typed client is generated from the
same spec with [Orval](https://orval.dev):

```bash
# List credentials
curl -H "Authorization: Bearer $FS_ADMIN_TOKEN" localhost:8090/api/v1/access-keys

# Create a credential scoped to buckets matching "uploads-*"
curl -H "Authorization: Bearer $FS_ADMIN_TOKEN" -H "Content-Type: application/json" \
  -d '{"grants":[{"bucket":"uploads-*","permission":"write"}]}' \
  localhost:8090/api/v1/access-keys
```

### Run as a systemd service

Generate a unit for `fs s3` — a per-user service by default, or a hardened
system service with `--user=false`:

```bash
# Install and enable a per-user service
fs systemd --install --config ~/fs.yaml
systemctl --user daemon-reload
systemctl --user enable --now fs
loginctl enable-linger "$USER"   # keep it running after logout

# Or emit a system unit
fs systemd --user=false --config /etc/fs/config.yaml | sudo tee /etc/systemd/system/fs.service
```

The unit wires `ExecReload` to `SIGHUP`, so `systemctl --user reload fs` performs
the hot credential/TLS reload.

## Distributed cluster mode (M3)

Set `storage.type: cluster` and every node runs the same binary: objects are
placed across the cluster's failure domains (rack → node → disk), written at
quorum, and readable from any node. A cluster needs a reachable **etcd** for its
control plane and a shared **cluster secret** for peer authentication.

```yaml
storage:
  type: "cluster"

cluster:
  node_id: "node-1"            # unique per node (or FS_CLUSTER_NODE_ID)
  rack: "rack-a"               # failure-domain label
  addr: ":7080"               # internal peer listener — never expose publicly
  advertise_addr: "10.0.0.1:7080"
  secret: "change-me-to-a-long-random-string"   # or FS_CLUSTER_SECRET, min 16 chars
  scheme: "rf2.5"              # rf2.5 | rf3 | ec:k,m (e.g. ec:4,2)
  disks:
    - { id: "d0", path: "/data/d0" }
  etcd:
    endpoints: ["http://10.0.0.9:2379"]
```

- **Replication schemes** trade storage overhead for fault tolerance: `rf2.5`
  (2.5×, survives one domain loss), `rf3` (3×, survives two), or `ec:4,2`
  (Reed-Solomon, 1.5×, survives any two shard losses). Override the scheme
  per bucket through the admin API.
- **Rebalancing & repair** run online: after a node joins or leaves, placement
  converges automatically, and a background scrubber repairs missing or corrupt
  fragments.
- **`fs admin`** runs a headless, control-plane-only process (no data path) that
  serves the cluster-wide status dashboard and drives rebalance through the same
  etcd election a data node uses.
- **Cluster-wide credentials** — set `auth.source: etcd` so access keys, grants
  and the public-read bucket list live in the control plane (secrets sealed by an
  HKDF key derived from the cluster secret) and every node hot-reloads any change
  with no restart. The default `auth.source: file` keeps credentials node-local.

See [findings/DESIGN.md](findings/DESIGN.md) for the design and
[docs/FAILURE-MODEL.md](docs/FAILURE-MODEL.md) for the durability and failure
model.

## Operations

- **Durability** — `storage.fsync` (`none` / `file` / `file+dir`, default
  `file`) controls fsync aggressiveness; writes are always crash-atomic (no torn
  object). A background scrubber (`integrity.scrub_interval`) detects bit-rot and
  can quarantine corrupt objects; `integrity.verify_on_read` checks each object
  before serving.
- **Health & readiness** — `/health` (liveness: the process is up) and `/ready`
  (readiness: storage is reachable, 503 otherwise). Prometheus `/metrics` and
  pprof are served on a separate listener (default `localhost:9464`,
  `METRICS_ADDR` to change).
- **Hot reload** — send **`SIGHUP`** to reload credentials and the TLS
  certificate from disk without a restart.

## Installation

```bash
go install github.com/go-faster/fs/cmd/fs@latest
```

Or build from source:
```bash
git clone https://github.com/go-faster/fs
cd fs
go build -o bin/fs ./cmd/fs
```

## Usage

### Quick Start

```bash
# Start S3 server with defaults
fs s3

# Show help
fs s3 --help
```

### Configuration

The server supports both YAML configuration files and command-line flags:

```bash
# Using YAML configuration
fs s3 --config config.yaml

# Using command-line flags
fs s3 --addr :9000 --root /var/lib/s3data

# Mix both (flags override config file)
fs s3 --config config.yaml --addr :9000

# Generate example configuration
fs s3 --generate-config > my-config.yaml
```

Run `fs s3 --generate-config` to produce a fully commented configuration template, and `fs s3 --help` for the list of flags.

### Example Configuration

```yaml
server:
  addr: ":8080"
  read_timeout: 30s
  write_timeout: 30s
  idle_timeout: 120s
  health_path: "/health"

storage:
  root: ".s3data"
  type: "filesystem"

observability:
  service_name: "go-faster/fs"
  enable_request_logging: true
  enable_metrics: true
  enable_tracing: true
```

## Use as a library

The S3 server is embeddable. Install the module and pick a storage backend —
[`storagefs`](storagefs) for the filesystem, [`storagemem`](storagemem) for
in-memory, or your own implementation of the [`fs.Storage`](storage.go)
interface:

```bash
go get github.com/go-faster/fs
```

The library core pulls in **no observability stack** — wrap the handler yourself
(e.g. with `otelhttp`) via `server.NewHandler` or `Config.WrapHandler`.

Custom backends can verify themselves against the storage contract with the
[`storagetest`](storagetest) conformance suite:

```go
func TestStorage(t *testing.T) {
	storagetest.Run(t, func(t testing.TB) fs.Storage {
		return mybackend.New(t.TempDir())
	})
}
```

### Mount the handler into your own server

Use `server.NewHandler` when you already run an `http.Server` or mux and just
want to expose the S3 API (optionally under a path prefix):

```go
package main

import (
	"net/http"

	"github.com/go-faster/fs/server"
	"github.com/go-faster/fs/storagefs"
)

func main() {
	store, err := storagefs.New("/data")
	if err != nil {
		panic(err)
	}

	mux := http.NewServeMux()
	mux.Handle("/s3/", http.StripPrefix("/s3", server.NewHandler(store)))

	http.ListenAndServe(":8080", mux)
}
```

### Run the turnkey server

Use `server.New` for a managed server with a health endpoint, request timeouts,
optional bucket pre-creation and graceful shutdown driven by a `context`:

```go
package main

import (
	"context"
	"os/signal"
	"syscall"

	"github.com/go-faster/fs/server"
	"github.com/go-faster/fs/storagemem"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	srv, err := server.New(server.Config{
		Storage: storagemem.New(),
		Addr:    ":9000",
		Buckets: []string{"uploads"}, // pre-created if absent
	})
	if err != nil {
		panic(err)
	}

	// Serves until ctx is canceled, then drains in-flight requests.
	if err := srv.ListenAndServe(ctx); err != nil {
		panic(err)
	}
}
```

### `server.Config`

| Field | Default | Description |
|-------|---------|-------------|
| `Storage` | — (required) | Backend serving S3 operations (`fs.Storage`). |
| `Addr` | `:8080` | TCP address to listen on. |
| `ReadTimeout` / `WriteTimeout` / `IdleTimeout` | `30s` / `30s` / `120s` | Underlying `http.Server` timeouts. |
| `HealthPath` | `/health` | Plaintext liveness endpoint; `"-"` disables it. |
| `ReadyPath` / `Ready` | `/ready` / — | Readiness endpoint and its probe; a non-nil probe error returns 503. |
| `Buckets` | — | Buckets created (idempotently) before serving. |
| `Auth` / `CORS` / `TLS` | — | SigV4 auth store, per-bucket CORS, and hot-reloadable TLS. |
| `WrapHandler` | — | Wrap the handler with middleware/observability (e.g. `otelhttp.NewHandler`). |

See the [`server` package reference](https://pkg.go.dev/github.com/go-faster/fs/server)
for the full API and runnable examples.

## Roadmap

Delivered so far: full SDK wire compatibility, exact S3 semantics and metadata,
SigV4 auth/authorization/TLS, canned ACLs, durability & integrity operations,
and the M3 distributed stack (etcd control plane, failure-domain placement,
replication schemes + erasure coding, repair, auto-rebalancing, cluster
observability, headless admin, and cluster-wide runtime key management).

Planned and in progress (see [findings/ROADMAP.md](findings/ROADMAP.md) for the
authoritative, detailed list):

- **Object versioning** — per-object version chains, delete markers, per-version
  tags/ACLs (the largest upcoming item).
- **Server-side encryption (SSE-S3)** — envelope AES-256-GCM at rest.
- **Lifecycle expiration** and, after versioning, noncurrent-version cleanup.
- **Embedded etcd** — in-process etcd for all-in-one 1/3-node clusters.
- **Virtual-host–style addressing**, **ACME / automatic TLS**, and **static
  website hosting**.
- **Geo-replication** — async bucket-level replication between clusters.
- **Bucket-policy subset** — demand-gated, when canned ACLs are not enough.

## Development

```bash
# Run tests
go test ./...

# Build
go build ./cmd/fs

# Run with coverage
make coverage
```

## License

Apache 2.0
