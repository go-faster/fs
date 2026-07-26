# A three-node cluster, under load

Everything the cluster needs and something to keep it busy: etcd, three fs
nodes, and a load generator writing and reading across four buckets at sizes
from a few KiB to tens of MiB.

```sh
docker compose up --build
```

The first build compiles the admin dashboard and the Go binaries, so it takes a
few minutes; after that it is cached.

| | |
|---|---|
| Admin dashboard | <http://127.0.0.1:8091> — token `demo-admin-token` |
| S3 | `http://127.0.0.1:9001` (also 9002, 9003) |
| Credentials | access key `demo`, secret `demodemodemo` |

Every node serves every key and every node's dashboard shows the whole cluster,
so 8092 and 8093 are the same view from a different machine's perspective.

## What to look at

The **Cluster** page is where the demo pays off. Under load you can watch:

- **Disk fullness** climbing per node, and the placement spreading writes evenly
  across the three failure domains.
- **Queue depth**, non-zero while a write's trailing replica or parity is still
  being produced behind the acknowledgement.
- **Scrub coverage** — how long ago the least recently checked object on each
  node was checked, or how many have never been checked. The scrub runs every
  five minutes here so the number actually moves.
- **Bucket usage** on the Buckets page: object counts and sizes per bucket,
  derived from the nodes' object indexes.

## Things worth doing to it

```sh
docker compose stop fs3          # one node down: reads and writes carry on
docker compose start fs3         # it rejoins and repair catches it up
docker compose logs -f loadgen   # what the workload is doing
```

Stopping a node is the interesting one. `rf2.5` places two full replicas and a
parity fragment across three failure domains, so one node down leaves every
object readable and writable; the dashboard shows the node as not reporting,
and the repair queue drains once it returns.

## The workload

Four buckets with deliberately different shapes, because one bucket of
identically sized blobs exercises almost nothing:

| Bucket | Object size | Share of traffic | Reads |
|---|---|---|---|
| `thumbnails` | 4–64 KiB | 40% | 80% |
| `documents` | 64 KiB–2 MiB | 30% | 60% |
| `logs` | 16–512 KiB | 20% | 10% |
| `media` | 8–48 MiB | 10% | 50% |

`media` objects are large enough that the client uploads them in parts, so
multipart is exercised too. Keys are nested under sixteen prefixes so that
delimiter listings have something to fold. Five percent of operations are
listings and five percent deletes.

Tune it in `docker-compose.yml`: `FS_RATE` is operations per second across the
whole generator and `FS_WORKERS` how many run concurrently. The defaults are
deliberately gentle; raise them to make the dashboard interesting or to push
the cluster.

## What it has already caught

The load generator is realistic on purpose, and that has paid for itself twice:

- A node told its identity only through the environment registered an **empty
  address**, so it started, reported healthy, and every peer failed to dial it.
  Validation accepted the environment variable; the code that registered the
  node read the config field.
- Every **multipart upload** from a client using streaming signatures failed
  against a cluster. Part uploads declared `Content-Length`, which counts the
  chunk framing, instead of the decoded length — so the backend was told to
  expect bytes that never arrived. Single-node backends copy until EOF and
  ignore the declared size, which is why nothing had noticed.

Both are fixed. Neither would have shown up under a workload of small,
identically sized objects written to one bucket through one node.

## Not a production deployment

- **One etcd member.** It holds the topology and the sealed credentials, so a
  real cluster runs three or five.
- **`fsync: none`.** An acknowledged write here does not survive power loss.
  Production uses `file+dir`.
- **Secrets in the compose file.** The cluster secret, the admin token and the
  S3 credentials are literals in version control.
- **One disk per node**, and no resource limits.

For a deployment that is meant to look like production, see
[`../kind/`](../kind/) and the Helm chart.
