# Deployment

go-faster/fs runs as a **single node** — one process, one data directory, no
redundancy beyond the underlying disk — or, experimentally, as a replicated
cluster (see [Cluster mode](#cluster-mode)). Both store data in the engine under
`<storage.root>/.engine/`.

**Upgrading from a filesystem-backend release.** The filesystem backend has
been removed and its data directory is not converted. A node started on a root
it wrote (one holding `.tmp`, `.meta`, `.multipart`, `.versions`,
`.quarantine` or any non-dot directory) refuses to start and says so. Copy the
objects out with the previous release (e.g. `aws s3 sync` / `mc mirror`) and
into a new storage root served by this one. This is a pre-v1 break: there is no
in-place migration.

One binary (`fs s3`) and one YAML config. `fs s3 --generate-config` prints a
fully-defaulted config to start from.

Contents: [systemd](#systemd) · [Docker](#docker) · [Docker Compose](#docker-compose)
· [Kubernetes / Helm](#kubernetes--helm) · [Security](#security)
· [Observability](#observability).

## systemd

`fs systemd` generates a unit; `--install` writes it. By default it emits a
per-user unit (`systemctl --user`); `--user=false` emits a hardened system unit.

```sh
# Hardened system-wide unit for a config-file deployment
fs systemd --user=false --config /etc/fs/config.yaml > /etc/systemd/system/fs.service
systemctl daemon-reload
systemctl enable --now fs
```

The generated unit uses `Restart=on-failure` (5 s), `ExecReload` on SIGHUP (hot
config/credential/TLS reload — no restart), and, for the system variant,
`DynamicUser`, `StateDirectory=fs`, `ProtectSystem=strict`, `ProtectHome`,
`PrivateTmp`, `NoNewPrivileges`. systemd stops the service with SIGTERM; the
binary bridges SIGTERM to a graceful drain, so `systemctl stop` and restarts
don't drop in-flight requests.

## Docker

The repository `Dockerfile` packages a **prebuilt** static binary on Alpine as
non-root user `fs` (uid/gid 1000), entrypoint `/usr/local/bin/fs`. Build the
binary first, then the image:

```sh
CGO_ENABLED=0 GOOS=linux go build -o fs ./cmd/fs
docker build -t go-faster-fs .

docker run --rm -p 8080:8080 -v fsdata:/data \
  go-faster-fs s3 --addr :8080 --root /data
```

Released images are published to `ghcr.io/go-faster/fs`.

## Docker Compose

`dev/observability/docker-compose.yml` stands up a **single node**
plus a full observability stack (Grafana, Prometheus, Tempo, Jaeger, Alloy). It
is a development/demo topology — the fs node uses a `tmpfs` `/data`
(ephemeral). `dev/observability/run.sh` builds the binary and brings the stack
up.

```sh
cd dev/observability && ./run.sh
```

Use it to explore the metrics/traces pipeline, not for durable storage.

## Kubernetes / Helm

The chart in `helm/go-faster-fs` deploys a **single-node** instance
(one StatefulSet replica). It is the right tool for a standalone S3 endpoint —
CI fixtures, dev/test backends, a single-box deployment.

Set `persistence.emptyDir: false` so a PVC (`volumeClaimTemplates`) backs the
data directory; the default `emptyDir` is **ephemeral** and loses data on pod
restart. Do **not** raise `replicaCount` or enable the HPA to "scale" it — extra
replicas are independent, non-replicating single nodes.

```sh
helm install fs ./helm/go-faster-fs \
  --set persistence.emptyDir=false \
  --set persistence.size=200Gi
```

See `helm/go-faster-fs/values.yaml` and `values-production.yaml` for the full
surface (ingress/HTTPRoute, TLS via cert-manager, resource requests, OTEL
exporters).

## Security

- **Authentication is off only when you disable it.** Provide `auth.keys` (or a
  root credential via `FS_ROOT_ACCESS_KEY` / `FS_ROOT_SECRET_KEY`). The compose
  and default Helm setups are insecure/anonymous for convenience — override for
  anything real.
- **TLS**: set `server.tls.cert_file` / `key_file` (hot-reloaded on SIGHUP or
  `POST /api/v1/reload`), or terminate TLS at an ingress.
- **Admin API** (credential management, config reload) listens separately
  (`admin.addr`, default `localhost:8090`) and requires a bearer token
  (`admin.token` or `FS_ADMIN_TOKEN`). Keep it bound to localhost or behind a
  proxy.
- **Hot reload without a signal**: `POST /api/v1/reload` re-applies the same
  hot-reloadable configuration SIGHUP does — the config-defined credentials and
  grants and the TLS certificate, preserving runtime-created keys — and returns
  what it reloaded and the config revision now in effect. Set an opaque
  `revision:` marker at the top of the config and read it back from
  `GET /api/v1/info` (`config_revision`) or the reload response to confirm a
  node has loaded a specific config, e.g. after an orchestrator rewrites it.
- **Encryption at rest (SSE-S3)**: `encryption.master_key_file` (or
  `FS_MASTER_KEY`) seals each object's own data key. To rotate: make the new
  key `master_key_file` and list the old one in
  `encryption.previous_key_files` on every node, restart, run
  `fs encrypt rotate` (`POST /api/v1/encryption/rotate`; one run covers a
  cluster) until it reports 0 remaining, then drop the old key. Rotation
  rewrites keys, never object data, and is safe to interrupt and rerun.
- **SSE-C** (customer-provided keys) needs no server configuration; the key
  never touches disk, and losing it loses the object. Like S3, keys are refused
  on plain HTTP — terminate TLS here or at a proxy that sets
  `X-Forwarded-Proto: https`. `encryption.customer_keys_over_http: true`
  lifts that, for development and tests only.

## Cluster mode

Experimental (#279). With `cluster.node_id` set,
each object's metadata and data live on three nodes of a layout applied with
`fs layout apply`; writes and reads need two of them. A node's engine data is
under `<storage.root>/.engine` (bbolt metadata + content-addressed blocks).
Peer traffic on `cluster.addr` (default `:7080`) is authenticated with the
shared `cluster.secret` (HMAC over every request and response) but **not
encrypted** — keep it on a private network and never expose it publicly. Each
node keeps its adopted layout in `<storage.root>/.cluster/layout.json`; a file
in an unknown format stops the node from starting rather than being misread.

**Erasure coding**, per bucket, instead of three copies: `ec:4,2` stores a
block as 4 data + 2 parity shards on six nodes — 1.5× the data, any two lost —
and `ec:2,1` fits three nodes at 1.5×, any one lost. Spread the layout for
the width first (`fs layout apply` with `widths: [3, 6]` for `ec:4,2`; with
three zones that also puts at most two shards in a zone, so losing a zone
loses no data), then `fs bucket scheme BUCKET ec:4,2` (or
`PUT /api/v1/buckets/{bucket}/scheme` `{"scheme": "ec:4,2"}`). Only new blocks of 256 KiB and up are coded: small
objects and short tail blocks stay replicated, and existing blocks keep the
scheme they were written with. A write needs K+1 shard holders up, a read
any K. A layout narrower than a bucket's code — current, or any it had
before, since its coded blocks stay — is refused.

## Observability

- **Health**: `/health` (liveness, always 200 once serving) and `/ready`
  (readiness, probes storage reachability → 200/503). Point Kubernetes/systemd
  liveness at `/health`, readiness at `/ready`.
- **Metrics**: OpenTelemetry via the SDK, enabled with
  `OTEL_METRICS_EXPORTER=prometheus` and served on
  `OTEL_EXPORTER_PROMETHEUS_HOST:PORT` (compose uses `:9464/metrics`).
- **Traces**: `OTEL_TRACES_EXPORTER=otlp` + `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`.
- **pprof**: set `PPROF_ADDR`.
- **Engine**: `fs.engine.blocks.resync_pending` —
  block copies a replica is missing, the number to watch —
  `fs.engine.blocks.corrupt`, `fs.engine.blocks.collected`,
  `fs.engine.blocks.degraded` (erasure-coded reads that rebuilt from parity),
  `fs.engine.shards.{lost,critical,degraded}` — erasure-coded blocks below K
  shards (unreadable), at K (one loss from it), short of a shard; sum them
  over nodes and alert on the first two — with
  `fs.engine.shards.{unreachable,rebuilt,handed_over,age}` for the repair
  sweep (cluster only),
  `fs.engine.sync.{out_of_sync,unreachable,age}{table}` (anti-entropy, cluster
  only), `fs.engine.gc.age`, and
  `fs.engine.tombstones.{queued,collected,deferred}{table}` — deleted rows
  wait a day, then go once every replica has them; `queued` climbing with
  `deferred` means a replica has been unreachable through collections.
- **Cluster** (when `cluster.node_id` is set): `fs.cluster.layout.version` —
  compare across nodes; one lagging means gossip is not reaching it — and
  `fs.cluster.peers{state=up|down}`. `fs layout nodes` shows the same per
  peer, with the last error.
- Toggle whole subsystems with `observability.enable_metrics` /
  `enable_tracing` / `enable_request_logging`.
