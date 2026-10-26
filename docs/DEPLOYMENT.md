# Deployment

go-faster/fs runs as a **single node** — one process, one data directory, no
redundancy beyond the underlying disk — or, experimentally, as a replicated
cluster (see [Cluster mode](#cluster-mode)). Both store data in the engine under
`<storage.root>/.engine/`.

A root written in a storage format this release does not read is refused at
startup, never misread. There is no migration between releases before v1.

One binary (`fs s3`) and one YAML config. `fs s3 --generate-config` prints a
fully-defaulted config to start from. A key the binary does not know —
misspelled, or a setting an earlier release had — stops it at startup with an
error naming the key, rather than being ignored.

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

## Virtual-host addressing

Path-style requests (`https://s3.example.com/photos/cat.jpg`) work on any
host. To also serve virtual-host-style ones (`https://photos.s3.example.com/cat.jpg`),
which some clients default to, list the service domains:

```yaml
server:
  domains: ["s3.example.com"]
```

(or `fs s3 --domain s3.example.com`, repeatable). Then:

- **DNS:** a wildcard record per domain, `*.s3.example.com`, pointing at the
  server, beside the record for `s3.example.com` itself.
- **TLS:** a certificate for both `s3.example.com` and `*.s3.example.com`. A
  bucket with dots in its name (`my.photos`) does not match a wildcard
  certificate; most SDKs fall back to path-style for such buckets over HTTPS,
  as they do against AWS.
- **Proxies** must pass the `Host` header through unchanged: nginx needs
  `proxy_set_header Host $host`; Caddy and Traefik pass it by default. SigV4
  signs `Host`, so a proxy that rewrites it breaks authentication anyway.
  `X-Forwarded-Host` is not read.
- A host that matches no domain — an IP address, an internal name — is served
  path-style, so health checks and clients addressing the server directly
  keep working. `/health` and `/ready` are answered only on such hosts and
  on the bare domains; on a bucket host they are object keys.
- The startup log names the domains in effect, and every request log line
  carries its `host`: a virtual-host request answered `NoSuchBucket` for the
  first label of its host points at a missing or misspelled domain.

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

**Changing the layout** (adding, removing or resizing nodes) is a transition,
not a switch. Until every node holding data has synced the new layout —
pulled what it now replicates, handed over what it no longer does — writes
also go to the replicas of the old layout and reads come from them, so nothing
acknowledged is missed while data moves. `fs layout show` says when a change
is still in transition and `fs layout nodes` shows each node's synced
version; `fs.cluster.layout.retained` above 1 for long means some node has
not synced it (down, or unable to reach a replica). **Keep a node you remove
running until the change completes**: it still serves reads for what it held.
If a node is gone for good and blocks a change from completing,
`fs layout skip NODE` releases it — whatever only it held is given up.

Background work runs on defaults that rarely need changing:
`storage.background.{sync_interval, resync_interval, gc_interval, gc_grace,
gc_walk_interval, tombstone_delay}` (10m, 10s, 1h, 10m, 168h, 24h). Block
collection checks the blocks released since its last pass; every
`gc_walk_interval` it checks every block a node holds instead, for those a
release never named (a node that crashed before telling the holders, a
holder unreachable at the time). A layout change does not wait for
the sync interval: a node sweeps as soon as it adopts one, and retries a
failed sweep after 5s, backing off to the interval. Shorter intervals repair
faster at the cost of sweeping more often; a tombstone delay shorter than the longest a
replica may stay unreachable risks deleted data coming back.

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

## Quotas

Ceph RGW-style quotas limit what a bucket, or an account — every bucket one
owner owns, together — stores, in bytes and objects:

```sh
fs quota bucket photos --max-size 100GiB --max-objects 1000000 --enable
fs quota account alice --max-size 1TiB --enable   # alice: the owner's user ID
fs quota bucket photos                            # limits and current usage
```

(or `PUT /api/v1/buckets/{bucket}/quota` and
`PUT /api/v1/accounts/{account}/quota` with
`{"enabled": true, "max_size": 107374182400, "max_objects": -1}`; `-1` is no
limit.) An account is the owner ID buckets record: the `user_id` of the key
that created them, defaulting to its access key; with auth off buckets have no
owner and only bucket quotas apply. A write that would pass an enabled limit
fails with `QuotaExceeded` (403); deletes and overwrites that do not grow
usage always succeed, so a bucket over a lowered limit can be cleaned up.
Usage counts every stored object version, noncurrent ones included, but not
delete markers or uploads in flight.

Enforcement is soft, as in RGW: each node trusts usage it read for
`storage.quota_cache_ttl` (default `10s`), so clients writing through several
nodes at once can overshoot a limit by what they store in that window. A
usage read that fails lets the write through
(`fs.engine.quota.lookup_failures`).

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
  only), `fs.engine.gc.{age,walk_age}` — since the hourly collection and
  the weekly walk of every block — `fs.engine.gc.candidates`, released
  blocks left to check again (growing: references unreadable), and
  `fs.engine.tombstones.{queued,collected,deferred}{table}` — deleted rows
  wait a day, then go once every replica has them; `queued` climbing with
  `deferred` means a replica has been unreachable through collections.
  Quotas: `fs.engine.quota.checks` and `fs.engine.quota.rejected{scope}`
  (bucket, account) — their ratio is how often clients hit a limit — and
  `fs.engine.quota.lookup_failures`, writes let through because usage could
  not be read.
- **Lifecycle**: `fs.lifecycle.age` — seconds since a pass last completed,
  the number to alert on (-1: none yet; a cluster's buckets are spread over
  its nodes, so each node reports its own) — `fs.lifecycle.failed_buckets`,
  buckets the last pass could not finish, and, counted since start,
  `fs.lifecycle.removed{action}` (`expired`, `noncurrent`, `marker`,
  `aborted`) and `fs.lifecycle.raced`, expired objects left alone because
  they changed before the delete.
- **Cluster** (when `cluster.node_id` is set): `fs.cluster.layout.version` —
  compare across nodes; one lagging means gossip is not reaching it —
  `fs.cluster.layout.retained` (layout versions in use: above 1 while a change
  is in transition, and for long when a node has not synced it),
  `fs.cluster.layout.synced` (the newest version this node has synced), and
  `fs.cluster.peers{state=up|down}`. `fs layout nodes` shows the same per
  peer, with the last error.
- **Access keys** (with auth on): `fs.auth.keys.managed` — keys created
  through the admin API this node accepts; compare across nodes — and
  `fs.auth.keys.refresh_age`, seconds since the node last read them: a key
  created through another node takes effect here no sooner, and a value well
  above 5 means this node cannot read the replicated metadata.
- Toggle whole subsystems with `observability.enable_metrics` /
  `enable_tracing` / `enable_request_logging`.
