# Deployment

go-faster/fs runs in two modes:

- **Single-node filesystem** (`storage.type: filesystem`) — one process, one data
  directory. Simple; no redundancy beyond the underlying disk.
- **Cluster** (`storage.type: cluster`) — 3–16 nodes coordinated through etcd,
  with replicated or erasure-coded placement across failure domains. See
  [SIZING.md](SIZING.md) and [FAILURE-MODEL.md](FAILURE-MODEL.md).

All modes share one binary (`fs s3`) and one YAML config. `fs s3
--generate-config` prints a fully-defaulted config to start from.

Contents: [systemd](#systemd) · [Docker](#docker) · [Docker Compose](#docker-compose)
· [Kubernetes / Helm](#kubernetes--helm) · [Cluster mode](#cluster-mode)
· [Security](#security) · [Observability](#observability).

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
binary bridges SIGTERM to a graceful drain and (in cluster mode) a clean etcd
deregistration, so `systemctl stop` and rolling restarts don't drop in-flight
requests — see [UPGRADE.md](UPGRADE.md).

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

`compose/docker-compose.yml` stands up a **single filesystem node** plus a full
observability stack (Grafana, Prometheus, Tempo, Jaeger, Alloy). It is a
development/demo topology — the fs node uses a `tmpfs` `/data` (ephemeral) and no
auth. `compose/run.sh` builds the binary and brings the stack up.

```sh
cd compose && ./run.sh
```

It is not a cluster (no etcd, one node). Use it to explore the metrics/traces
pipeline, not for durable storage.

## Kubernetes / Helm

The chart in `helm/go-faster-fs` deploys a **single-node filesystem** instance
(one StatefulSet replica). It is the right tool for a standalone S3 endpoint —
CI fixtures, dev/test backends, a single-box deployment.

Set `persistence.emptyDir: false` so a PVC (`volumeClaimTemplates`) backs the
data directory; the default `emptyDir` is **ephemeral** and loses data on pod
restart. Do **not** raise `replicaCount` or enable the HPA to "scale" it — extra
replicas are independent, non-replicating filesystem nodes, not a cluster.

```sh
helm install fs ./helm/go-faster-fs \
  --set persistence.emptyDir=false \
  --set persistence.size=200Gi
```

See `helm/go-faster-fs/values.yaml` and `values-production.yaml` for the full
surface (ingress/HTTPRoute, TLS via cert-manager, resource requests, OTEL
exporters).

**Clustered Kubernetes deployment is the job of a dedicated operator**, not this
chart — coordinating per-pod identity, etcd, failure-domain-aware placement, and
rolling upgrades is stateful-operator territory. The binary is ready for it: it
reads its per-instance identity from the environment
(`FS_CLUSTER_NODE_ID`, `FS_CLUSTER_ADVERTISE_ADDR`, `FS_CLUSTER_SECRET`), so an
operator can drive a StatefulSet from one shared config plus the downward API.
See [Cluster mode](#cluster-mode) for the config an operator renders per node.

## Cluster mode

Every cluster node needs, at minimum (`cluster` config section):

| Field | Required | Notes |
|---|---|---|
| `node_id` (or `FS_CLUSTER_NODE_ID`) | yes | Unique per node; the env form lets an orchestrator inject a per-pod identity into a shared config. |
| `advertise_addr` (or `FS_CLUSTER_ADVERTISE_ADDR`) | yes | `host:port` peers dial; the bind `addr` defaults to `:7080`. Env-overridable per instance. |
| `secret` (or `FS_CLUSTER_SECRET`) | yes | Shared peer-auth secret, ≥16 chars. |
| `etcd.endpoints` | yes | The control plane; run a real etcd cluster (3/5 nodes). |
| `scheme` | no | `rf2.5` (default), `rf3`, or `ec:k,m`. |
| `rack` | no | Failure-domain label; placement spreads copies across racks first. |
| `disks` | no | One or more `{id, path, weight}`; default one disk under the root. |

Set `storage.type: cluster`. Give nodes distinct `rack` labels when they share
real fault domains (racks, availability zones) so replicas land on independent
hardware — see [FAILURE-MODEL.md](FAILURE-MODEL.md). Bring nodes up one at a
time; the first stamps the cluster schema version, the rest join and the
auto-rebalancer converges placement.

Minimal per-node `config.yaml`:

```yaml
server:
  addr: ":8080"
storage:
  type: cluster
cluster:
  node_id: node-a
  rack: rack-1
  advertise_addr: "10.0.0.1:7080"
  secret: "change-me-to-a-long-random-secret"
  scheme: "rf2.5"
  disks:
    - id: d0
      path: /data/d0
  etcd:
    endpoints: ["http://10.0.0.10:2379", "http://10.0.0.11:2379", "http://10.0.0.12:2379"]
```

## Security

- **Authentication is off only when you disable it.** Provide `auth.keys` (or a
  root credential via `FS_ROOT_ACCESS_KEY` / `FS_ROOT_SECRET_KEY`). The compose
  and default Helm setups are insecure/anonymous for convenience — override for
  anything real.
- **TLS**: set `server.tls.cert_file` / `key_file` (hot-reloaded on SIGHUP), or
  terminate TLS at an ingress.
- **Admin API** (credential management + rebalance control) listens separately
  (`admin.addr`, default `localhost:8090`) and requires a bearer token
  (`admin.token` or `FS_ADMIN_TOKEN`). Keep it bound to localhost or behind a
  proxy.
- **Cluster secret** authenticates all peer traffic (HMAC). Treat it like a
  password; supply it via `FS_CLUSTER_SECRET` / a Kubernetes Secret, not in a
  committed file.

## Observability

- **Health**: `/health` (liveness, always 200 once serving) and `/ready`
  (readiness, probes storage reachability → 200/503). Point Kubernetes/systemd
  liveness at `/health`, readiness at `/ready`.
- **Metrics**: OpenTelemetry via the SDK, enabled with
  `OTEL_METRICS_EXPORTER=prometheus` and served on
  `OTEL_EXPORTER_PROMETHEUS_HOST:PORT` (compose uses `:9464/metrics`). Cluster
  nodes export `fs.cluster.*` metrics (per-disk capacity, placement skew, repair
  queue depth, rebalance progress, scrub totals) — see [PERFORMANCE.md](PERFORMANCE.md).
- **Traces**: `OTEL_TRACES_EXPORTER=otlp` + `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`.
- **pprof**: set `PPROF_ADDR`.
- Toggle whole subsystems with `observability.enable_metrics` /
  `enable_tracing` / `enable_request_logging`.
