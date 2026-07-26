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
lower a disk's weight to migrate data off it. Weight changes are a membership
change the auto-rebalancer converges automatically.

**A weight that is not positive drains the disk**: `weight: 0` works, and so
does a negative one. Placement skips any disk whose weight is not positive.

(Before v0.12.0 `weight: 0` did the opposite of what it says. An omitted key
and an explicit `0` were the same value in the config, so `0` was read as
"unset" and became the default 1 — full weight, on a disk the operator had
asked to empty. Only a negative weight drained. If you worked around it with
`-1`, that still works.)

### Draining without a restart

Editing a config file drains a disk only after the node restarts to read it.
`fs cluster drain` does it through the control plane instead, taking effect
across the cluster within seconds:

```sh
# What is currently overridden
fs cluster drain --config config.yaml

# Take a disk out of placement before pulling it
fs cluster drain node-a d1 --reason "failing SMART" --config config.yaml

# Shift half its load elsewhere without emptying it
fs cluster drain node-a d1 --weight 0.5 --config config.yaml

# Put it back at its configured weight
fs cluster drain node-a d1 --undrain --config config.yaml
```

The override is control-plane state, not the node's, and that is what makes it
survive a restart: a node republishes its own registration on every capacity
refresh, so a weight written there by anyone else would be undone within the
refresh interval — and a drained disk would silently return to placement.

A disk can be drained while its node is down, which is often exactly when an
operator wants to. An override for a disk that never appears does nothing.

The same thing is available over the admin API as
`PUT /api/v1/cluster/disk-weights/{node}/{disk}`, which is what an orchestrator
uses.

### Knowing when a drain has finished

Weight says a disk was taken out of placement; it does not say its data has
finished moving. `GET /api/v1/cluster/status` reports `has_data` per disk — false
once the disk holds no fragments, which is when it is safe to remove.

Do not infer this from capacity. `total_bytes`/`free_bytes` come from `statfs`,
so they describe the whole filesystem: a disk holding no fragments still reports
bytes in use, and no threshold reliably means "empty". `has_data` is answered by
the node itself, exactly.

`has_data` is **absent**, not false, when the node did not report live state
(unreachable, or running a binary that predates the field) or could not read the
disk — `data_error` then says why. Absent means unknown, and treating unknown as
drained is how a disk still holding the only copy of something gets deleted.

### Watching a drain make progress

The same response carries `fragments` and `bytes` per disk: how much the disk
still holds, from the node's own occupancy index. They answer "how much longer",
which `has_data` cannot — poll them to see a drain converge instead of watching
a boolean that flips at the very end.

They are **progress, not the verdict**. The index is anchored by a full scan and
then maintained incrementally by the write path, so a write racing a scan can
leave it off by a little; `has_data` stays the thing to gate a volume deletion
on, because it is exact. `bytes` counts payload only, so it is smaller than the
used space `total_bytes`/`free_bytes` imply — those include filesystem overhead.

Both are **absent** until the node has anchored the index. A node adopts its
counters instantly across an orderly restart; after a crash it walks its disks
first, and reports nothing while it does. Absent means "not counted yet" — never
"empty".

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

## Knowing what a bucket holds

`GET /api/v1/buckets/usage` reports each bucket's object count and total size,
plus the cluster-wide totals. Sizes are logical — what clients stored, not what
the disks hold; multiply by the bucket's scheme overhead (see [Raw capacity per
scheme](#raw-capacity-per-scheme)) for the physical figure.

The counters are a **durable index**, not a computed answer. Counting on demand
means a scatter-gather over every disk that reads every sidecar — the same walk
a listing does — which is precisely unusable at the object counts above. So each
write and delete reports its delta, one node folds the batches into the etcd
control plane, and the totals are read straight back out.

Between recounts a total can drift. A node that dies after committing a write
but before reporting it leaves its bucket short; a delete that half-failed
leaves it long. A cluster-wide recount re-derives every bucket's totals from the
objects themselves, on one elected node, and replaces the drifted figures —
carrying forward whatever was accounted while it ran.

The recount reads the nodes' object indexes, which costs index pages rather than
a scan of every disk and a read of every sidecar, so it runs **hourly**. A
cluster whose indexes cannot answer — one node still building its own, or
running a binary without one — falls back to the walk and to **every 6 hours**,
because that pass is expensive enough that doing it four times as often would
cost more than the drift it removes.

Read `counted` to know how much to trust a number: it is when a recount last
verified that bucket, and it is **absent** for a bucket whose totals have only
ever been maintained incrementally. `updated` is the last delta.

Multipart uploads are counted only when they complete. Parts in flight are not
objects — S3 does not list them — so an abandoned upload does not inflate a
bucket's usage, though it does occupy disk until the scrubber sweeps it.

## Scrub coverage

The scrubber verifies payloads against their recorded checksums, so a pass over
a disk reads all of it: hours for a large disk, and the sweep runs disk by disk.
That makes **restarts, not throughput, the thing most likely to stop it** — a
rolling upgrade or a crash lands mid-pass far more often than not.

Progress is therefore recorded on each disk as it goes (`.scrub.json` at the
disk root, written every few hundred objects), and a restarted node resumes
where it stopped instead of re-verifying the front of the disk. Disks are swept
interrupted-first, then least-recently-completed, so a node that restarts more
often than a full sweep takes still works its way through every disk rather than
covering the first few forever.

The number worth watching is **how long ago each disk was last completely
swept**, not how much repair work the scrubber has done — counters of work say
nothing about the objects a pass never reached. A pass that is interrupted never
stamps completion, so that timestamp only ever means what it says.

`GET /api/v1/cluster/status` reports the same thing per object rather than per
disk: `oldest_verified` is when the least recently checked object on a node was
checked, and `never_verified` how many have not been checked at all. With
`never_verified` at zero, `oldest_verified` is the honest age of that node's
coverage — every object it holds has been verified at least since then. A cycle
failing to keep up shows as that timestamp receding, or as a `never_verified`
that does not fall.

They are absent, not zero, when a node cannot derive them — a missing index, or
one still building. Zero would read as "holds nothing, everything just
verified", which is the reading these numbers exist to prevent.

Losing the cursor file costs a repeated pass and nothing else: it is progress,
not durability state, and a disk that cannot record it is still scrubbed.

The sweep streams names rather than listing them. It used to ask for every name
on the disk at once — several strings per object, so gigabytes before the first
one could be looked at, and the first thing to fail on a dense node. Names now
arrive in order, which makes an object's entries contiguous, so each is handled
and dropped before the next begins and memory tracks the largest single object
rather than the disk.

What remains proportional to the node is the set of objects already swept in the
current pass, kept to avoid repairing an object twice when two of a node's disks
hold it. Budget for it on nodes with very high object counts.

## The node object index

Each node keeps a local index of the objects its disks hold, under
`<storage.root>/cluster/index`. It exists because the only cluster-wide record
of an object is its sidecar, so listing a bucket, counting it, or sweeping it
otherwise means reading every sidecar on every disk — the walk that does not
finish at the object counts above.

Budget roughly **200 bytes per object held**, plus the log-structured store's
own overhead. A node at the 100M-object target should expect tens of GB. It can
live on a data disk or a separate faster one; it is rebuildable either way, so
the choice is about performance, not durability.

The index is **derived, never authoritative**. Sidecars remain the commit point,
which is what keeps a disk interpretable on its own: move it to another chassis
and every fragment still carries its bucket, key, size and scheme. Losing the
index costs a rebuild from those same disks and nothing else.

That is also why index writes are not synced by default: a node that stops
uncleanly rebuilds rather than trusting counters a crash may have swallowed. A
deployment that would rather pay a write-ahead-log fsync per object than rebuild
after a crash can have the index durable instead — the trade is one knob, and it
removes the staleness window without moving the commit point.

Nothing reads the index yet. Listings, usage and the scrub still take their own
walks; they move onto it in later work, and will consult its state first, the
way per-disk occupancy reports "not counted yet" rather than a confident zero.

## Listing cost

A listing is served from the nodes' object indexes: each node answers a page
from its own index in key order, and the coordinator merges those pages. A page
therefore costs what the page contains — a bounded number of entries from each
node — rather than a scan of every disk and a read of every sidecar.

Delimiters fold in the node, not above it, so a listing with `delimiter=/` over
a prefix holding a million keys costs a seek past that prefix rather than a read
of everything under it.

The cluster falls back to reading the sidecars when the indexes cannot answer:
a node still building one, or running a binary without one. That path is
correct and unchanged — it is what every listing did before — and it costs the
whole bucket per page, so a cluster that has fallen back will feel it on large
buckets. A rolling upgrade passes through that state and leaves it once every
node has indexed its disks.

An unreachable node does **not** force the fallback. Every object is indexed by
each node holding a copy of it, so a listing stays complete while every object
has one reachable holder — the same availability bound reads already have.
