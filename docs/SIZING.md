# Sizing

How to plan capacity for a go-faster/fs cluster: nodes, disks, the storage
overhead of each scheme, and the supporting etcd/network/memory footprint.

## Supported envelope (NFR-4)

| Dimension | Target |
|---|---|
| Nodes | 3–16 (single datacenter) |
| Objects per node | ≥ 100 million |
| Buckets | 10,000 |
| Max object size | 5 TiB |
| Parts per multipart upload | 10,000 |

Beyond ~16 nodes or across datacenters is out of scope for this design.

## Raw capacity per scheme

Usable capacity is raw disk divided by the scheme's storage overhead. Pick the
scheme per bucket (`fs cluster scheme <bucket> <scheme>`).

| Scheme | Overhead | Raw for 1 TiB usable | Min failure domains | Tolerates |
|---|---|---|---|---|
| `rf2.5` (default) | 2.5× | 2.5 TiB | 3 | 1 failure domain |
| `rf3` | 3.0× | 3.0 TiB | 3 | 2 failure domains |
| `ec:4,2` (RS(4,2)) | 1.5× | 1.5 TiB | 6 | 2 failure domains |
| `ec:k,m` | (k+m)/k | usable × (k+m)/k | k+m | m failure domains |

"Failure domain" is a rack (`cluster.rack`), or the node itself when no rack is
set. A scheme needs at least as many distinct domains as it has fragments, so
`ec:4,2` needs **6** independent domains — plan node/rack counts against the
scheme you intend to run. See [FAILURE-MODEL.md](FAILURE-MODEL.md).

**Worked example.** 200 TiB usable at `rf2.5` → 500 TiB raw. Across 10 nodes
that is 50 TiB raw per node, e.g. 4 × 16 TiB disks per node. For `ec:4,2` the
same 200 TiB usable needs only 300 TiB raw, but at least 6 nodes/racks.

## Headroom and the fullness watermark

Do not plan to fill disks. Leave headroom for:

- **Rebalancing and repair**, which copy data before deleting the old copy
  (never below the protection level mid-move) — transient extra usage.
- **Node loss**: when a node is down, its share of new writes and re-replication
  lands on the survivors.

Keep steady-state utilization comfortably under the fullness watermark
(`cluster.rebalance.full_watermark`, default 0.9); crossing it logs a warning and
raises the `fs.cluster.disk.fullness` / `placement.skew` metrics — the signal to
add capacity or lower a full disk's weight (drain). A practical target is
**≤ 70–75 %** steady-state so a node loss doesn't push survivors over the line.

## Weights and heterogeneous disks

Placement is weighted-HRW: a disk receives data in proportion to its `weight`
(default 1). Set weights proportional to capacity when disks differ in size, and
lower a disk's weight (toward 0 = drain) to migrate data off it. Weight changes
are a membership change the auto-rebalancer converges automatically.

## etcd

etcd holds only control-plane state (node registry, rebalance/migrate cursors,
schema version) — kilobytes, not object data — so it is CPU/IO-light. Run a
dedicated, odd-sized etcd cluster (**3 nodes** typical, 5 for larger clusters) on
fast disks; it is the cluster's coordination dependency, so give it the same
availability care as any etcd deployment. A single fs cluster maps to one etcd
key prefix (`cluster.etcd.prefix`, default `/fs`).

## Memory and CPU

- fs streams object bodies end to end — memory per request is bounded and does
  **not** scale with object size (verified in [PERFORMANCE.md](PERFORMANCE.md)),
  so large objects do not drive memory. Size RAM for connection concurrency and
  the OS page cache (which is what makes reads fast).
- Large-object PUT throughput is bounded by MD5 (the S3 ETag), ~0.75 GB/s per
  core, not the disk — see [PERFORMANCE.md](PERFORMANCE.md). Provision cores for
  the aggregate write throughput you need.
- EC read reconstruction and repair are CPU work (Reed–Solomon); budget cores on
  nodes running EC buckets under heavy repair.

## Network

- Coordinated writes cost one intra-DC round trip (synchronous W=2 quorum), so
  keep inter-node latency low (same DC). The design assumes low-latency links;
  cross-DC placement is out of scope.
- A write fans its fragments out to the placement targets; provision inter-node
  bandwidth for the write rate × the scheme overhead, plus rebalance/repair
  traffic (throttle the latter with `fs cluster rebalance --rate`).

## Object count

Each object is a small directory of fragment files plus a replicated sidecar.
Budget inodes/metadata for ≥ 100 M objects per node; listing is scatter-gather
with bounded memory (no full-bucket materialization), so listing large buckets is
IO- and fan-out-bound, not memory-bound.
