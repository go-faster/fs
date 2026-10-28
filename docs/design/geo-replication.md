# Design: geo-replication

**Status:** implemented: phases 1 (the outbox), 2 (the target), 3 (the
runner, admin API and CLI), 4 (backfill), 5 (the status header), 6 (the
chaos run) and 7 (the WAN soak). Required by [COMPATIBILITY.md](../../COMPATIBILITY.md)
("Planned: each requires a design document before commitment").

**Scope:** asynchronous, bucket-level replication of object versions from a
bucket on one fs deployment to a bucket on another. The deployments are
independent: each has its own layout, credentials, master keys and lifecycle.
One-way and two-way (each side replicating to the other) are both covered.
Replication to anything other than fs (AWS S3, MinIO) is
[out of scope](../../COMPATIBILITY.md#out-of-scope) and stays that way.

## Problem

A cluster survives the loss of nodes, racks and zones. Its zones are one
failure domain at the scale of a region, though:

- every write waits for a quorum, so zones must be close enough for
  synchronous round trips;
- one layout, one set of credentials and one master key ring cover all of
  it, so a mistake or a compromise reaches every zone at once.

An operator who needs a copy in another region, or a copy that one
deployment's failure or misconfiguration cannot destroy, has no way to get
one today except copying through the S3 API. Copying from outside loses
version IDs, timestamps and owners, re-encrypts everything, and needs its own
bookkeeping to know what it has copied.

## S3 semantics

Taken from AWS's documentation of S3 Replication (Same- and Cross-Region,
configuration V2). fs follows them except where this section says otherwise.

- **Versioning is required** on the source and the destination bucket.
  Replication is of versions, not of keys.
- **Replicated:** new versions written after the rule exists, with their
  version IDs, `Last-Modified`, metadata, tags, ACL and checksums; changes to
  a version's tags and ACL; SSE-C objects (as ciphertext); delete markers,
  when `DeleteMarkerReplication` is enabled.
- **Not replicated:**
  - deletes of a specific version (`DELETE ?versionId=`), so that a delete
    at the source cannot destroy the copy;
  - lifecycle actions, including the delete markers they create: each bucket
    runs its own lifecycle;
  - objects that existed before the rule (S3 Batch Replication copies those);
  - replicas, so chains (A→B→C) need a rule on B, and B only forwards what
    is written to it directly.
- **Status:** `x-amz-replication-status` on GET and HEAD is `PENDING`,
  `COMPLETED` or `FAILED` on the source, and `REPLICA` on the destination.
- **Two-way:** a rule on each side. Replica modification sync carries tag
  and ACL changes made on a replica back to the source.

## Design

### Overview

Replication ships **state**, not operations. A version is a CRDT entry of
the objects table (`meta.Version`); a destination that merges a shipped
version into its row converges with the source whatever the order, timing
or number of deliveries. Blocks are named by their SHA-256, so the
destination is asked which it lacks and only those cross the link.

```
source                                         destination
──────                                         ───────────
PUT key ─► objects row + outbox row (one table.Write)
             │
runner (one per bucket, elected)
  ├─ reads outbox page, versions, at quorum
  ├─ POST ?x-fs-replica=blocks  {hashes} ───►  which are missing?
  ├─ PUT  ?x-fs-replica=block   (each missing) ►  verify hash, store
  ├─ POST ?x-fs-replica=versions {versions} ─►  merge into objects row
  └─ marks outbox rows shipped
```

### Configuration

A bucket's replication is a bucket setting, `replication`, one LWW register
like `scheme` and `quota`, set through the admin API and `fs replication`:

```yaml
targets:
  - name: dr                            # unique within the bucket
    endpoint: https://s3.eu.example.com # the destination's S3 listener
    bucket: photos-replica
    access_key_id: FSRPL...
    secret_access_key: ...              # never returned by GET
    prefix: ""                          # optional key prefix filter
    delete_markers: true                # DeleteMarkerReplication
    insecure: false                     # allow http:// (tests only)
```

- **Several targets per bucket**, as AWS allows several rules with distinct
  destinations. Each target ships and keeps its progress independently.
- **The filter is a prefix.** Tag filters are a later addition, if asked
  for.
- **Validation on set:**
  - the source bucket has versioning Enabled; suspending versioning on a
    replicated bucket is refused, as AWS refuses it;
  - the target answers the handshake (below): reachable, credentials
    accepted with the `admin` grant on the target bucket, versioning
    Enabled;
  - `https://` unless `insecure`, because the protocol carries data keys.
- **The secret lives in the bucket row,** as access keys created through the
  admin API live in the `access_keys` table: replicated metadata,
  protected like the rest of it. The admin API never returns it.

### Capturing changes: the outbox

A new table, `replication`, partition key the bucket's ID, sort key
`key NUL versionID`. Its row is per version:

```go
type Outbox struct {
	Targets []string         // the targets the version is for, sorted
	Pending int64            // TS of the newest change to ship
	Version bool             // the version itself ships, not only its attrs
	Size    int64            // for the backlog's bytes
	Shipped map[string]int64 // target name → Pending as last delivered
	Refused map[string]Refusal // target name → its latest refusal
}
```

- **Merge:** the union of `Targets`, `or` of `Version`, `max` of
  `Pending`, `Size` and, per target, `Shipped`, and per target the later
  `Refused` (by the change refused, then when, then the reason). Commutative, associative
  and idempotent, so a change and a delivery can land in either order on
  any replica.
- **A version is pending for a target** that `Targets` names while
  `Shipped[target] < Pending`.
- **`Version`** tells the runner to ship the version whole the first time
  (`Shipped[target]` unset); an attrs change after the row was collected
  recreates it without `Version`, and ships the attrs alone.
- **Written in the same `table.Write`** as the change it records, so an
  acknowledged write always has its outbox row. Nothing is lost to a crash
  between the two. The changes that write it, in a bucket with at least one
  target:
  - a completed version (PUT, CopyObject, CompleteMultipartUpload);
  - a delete marker, unless it came from the lifecycle sweep (the sweep
    marks its context, and the engine skips the outbox for it);
  - a change of a version's tags or ACL (`Pending` moves to the change's
    TS).
- **Not written** for version deletes, lifecycle deletes, or writes applied
  as replicas.
- **Compaction:** a row every target it names has shipped is deleted,
  through the tombstone collection the other tables use. When a target is
  removed from the setting, the runner marks its rows shipped, so they do
  not wait for it.
- **Usage tally:** the table counts pending rows and bytes per bucket
  (`CountUsage`), so the backlog is one quorum read, not a scan.

The outbox key is per version, not per key, on purpose. Shipping a key's
whole version list on every change would re-ship versions the destination
deleted and has since collected; the tombstone is gone, so the merge would
bring them back.

### Running it

- **One runner per bucket,** elected as the lifecycle sweeper is
  (`sweepsBucket`): the first live node of the partition of the bucket's
  name. A brief overlap between two runners costs duplicate shipments, and
  shipments are idempotent.
- **Each pass**, per target: read a page of the bucket's outbox partition
  at quorum, keep the rows pending for the target, read their versions, and
  ship:
  1. **blocks:** ask the destination which of the versions' block hashes it
     lacks, read those (decoding erasure-coded ones) and send them, several
     in flight;
  2. **versions:** send the batch, and record `Shipped` for every version
     the destination accepted.
- **Pacing:** the runner polls every five seconds, and runs passes back to
  back while they ship anything. A pass first reads the bucket's backlog
  tally, one quorum read, and scans the outbox only when it is not zero.
  The scan reads rows already shipped until tombstone collection drops
  them, a day later; a time-ordered index is the fix if a busy bucket's
  passes show up as lag. A nudge from the coordinator was considered and
  left out: the poll bounds the delay at five seconds already.
- **Failures** back off per target, doubling from a second up to a minute.
  A version the destination refuses (below) is recorded in its outbox row
  (`Refused`, with the reason) and retried every minute, so one bad version
  never blocks the rest. A block the source cannot read refuses the version
  the same way, with a `source:` reason.

### The wire protocol

Served on the destination's S3 listener as a bucket subresource,
`?x-fs-replica`. It is authenticated with SigV4 and requires the `admin`
grant on the bucket. Being a subresource, it gets TLS, proxies,
virtual-host addressing, owner isolation and grants unchanged. AWS never
defines `x-fs-*` names. Other servers answer it with an error, which is what
the handshake reports.

| Request | Body | Answer |
|---------|------|--------|
| `GET /{bucket}?x-fs-replica` | — | protocol version, versioning status, whether a master key is configured |
| `POST /{bucket}?x-fs-replica=blocks` | a version ID and its blocks (hash, size) | per block: the scheme the target stores it with, and whether it is missing |
| `PUT /{bucket}?x-fs-replica=block&version=V&hash=H&scheme=S` | block bytes | stored; refused (`BadDigest`) if the bytes do not hash to `H` |
| `POST /{bucket}?x-fs-replica=versions` | versions (JSON) | per version: applied, or refused with a reason |

**Claims.** Asking for the missing blocks is also claiming them: the target
first writes the block references the version will hold (keyed by its
version ID), so collection keeps every block of the version from then on,
however long the rest of the shipment takes. A block reference only ever
turns deleted, so a version deleted at the target and shipped again stays
unreferenced, and the merge keeps it deleted. A version that never arrives —
its target removed mid-shipment — leaves its claims and their blocks
behind. Only a replicated block is looked up (`block.Manager.Has`, any
replica's copy); an erasure-coded one is always asked for, since how a
block is already stored at the target is not recorded anywhere a lookup
could find it. The scheme comes from the target and travels back with the
block and the version, so a scheme change mid-shipment cannot make a
version describe its blocks wrongly.

A shipped version carries what `meta.Version` and the engine's payload
carry, in a wire form the destination translates:

- key, version ID, TS, delete marker;
- size, ETag, `Last-Modified`, metadata, owner, checksums, part layout;
- tags and ACL, with the TS of the attrs register;
- inline content, or the block hashes and sizes;
- encryption, if any (below).

Applying it:

- **The destination merges the version** into its row under the key's
  lock, through the ordinary commit path. It writes the block references,
  releases what the merge replaces, and moves the usage tally. A version it
  already holds merges to the same row. A version that is `Gone` at the
  destination stays gone, since the further state wins.
- **Block placement is the destination's:** blocks are stored by its
  bucket's scheme (`ec:K,M` or replicated), and the payload's block
  locations are rewritten to it.
- **Blocks stored ahead of their version** are unreferenced until it lands.
  They are protected by the grace period that already covers a PUT's blocks
  before its commit.
- **An attrs-only change** applies only to a version the destination holds.
  It never creates one.
- **Replica writes are not quota-checked.** They count toward the
  destination's usage, but refusing one would turn a quota into a queue
  that never drains, of writes the source already acknowledged. The
  backlog metric shows the effect instead.
- **The destination marks the version a replica** (a payload flag), which
  makes its writes skip the outbox and makes `x-amz-replication-status:
  REPLICA` possible.
- **Refusals:**
  - the bucket is not versioned;
  - an encrypted version reaches a destination with no master key;
  - a block it references is missing.

### Encryption

Blocks of an encrypted version are ciphertext, sealed by the version's data
key, so they ship as they are, and dedupe still works on them.

- **SSE-S3:** the source unwraps the data key with its key ring and sends
  it in the version over TLS. The destination wraps it with its own key
  ring. Master keys are never shared between deployments.
- **SSE-C:** the wrapped key and the key ID ship unchanged. Neither side
  can decrypt, and both check the client's key the same way.

The protocol carries data keys, so it refuses `http://` targets unless
`insecure` is set, and the handshake checks the destination has a key ring
before any encrypted version is sent.

### Existing objects: backfill

Setting a new target (a name the bucket did not have, or one now pointing at
another endpoint, bucket or prefix), or `fs replication backfill`, starts a
walk of the bucket's versions that writes an outbox row for the target
(`Pending` = the later of the version's and its attrs' TS, `Version` set)
for each complete version under its prefix, delete markers when it takes
them. The runner ships those rows like any other. It is a background task:

- **its state is two bucket settings per target**, each with one writer:
  the request (its time, written by setting the target or asking again)
  and the walk's progress (request walked for, cursor, counts, written by
  the runner after every page). A request newer than the progress starts
  the walk over; an overlap of two runners walks a page twice, which only
  rewrites the same rows;
- **its cursor is persisted**, so it resumes after a restart or a runner
  change;
- **it starts a minute after the request**, so every write that committed
  with settings from before it has landed. A PUT reads the bucket before
  its body arrives, so one whose body took longer than ten seconds reads
  the settings again before committing: no write commits with settings
  older than that, and the walk cannot pass a key before a write to it
  that missed the target lands;
- **the runner walks ten pages of a thousand keys per pass** and target,
  then ships, so a long walk does not hold deliveries back.

A walk does not know what the target holds, so it queues every version.
Blocks the target holds are not sent again (claims dedupe them) and versions
it holds merge to themselves, but a version the target has deleted by
version and collected comes back: what a backfill means. In two-way
replication a backfill also ships the other side's replicas back to it,
which costs claims and changes nothing.

### Two-way replication

A rule on each side. Versions written on A ship to B and the reverse.
Replicas never re-enter the outbox, so nothing loops. Tags and ACLs changed
on a replica are written to the outbox like any other change, which is
AWS's replica modification sync.

Conflicts resolve the way they do inside a cluster:

- two versions of a key written on different sides are both kept, and the
  newer TS is current on both;
- concurrent attrs changes go to the later register.

### Status header

`x-amz-replication-status` on GET and HEAD of a version a target takes (its
key under the target's prefix), or of a replica:

| Value | Meant |
|-------|-------|
| `FAILED` | a target refused the version's latest change (it is retried); wins over the others |
| `PENDING` | a target has yet to receive the latest change, or the version has no outbox row and a target's backfill is still to walk |
| `COMPLETED` | every target has it |
| `REPLICA` | the version arrived through replication (its payload's `Replica` flag) |

`COMPLETED`, not `COMPLETE`: the AWS SDKs list both, and the S3 User Guide
documents `COMPLETED`, also for several destinations. A version with no row
and no backfill due is `COMPLETED`: its row was shipped and collected. It
costs one outbox read per request, only in a bucket with a target for the
key; a read that fails leaves the header out rather than failing the GET.

## Observability

Replication is a queue that falls behind silently, so each part gets a pair
of metrics, by bucket and target:

| Metric | |
|--------|-|
| `fs.replication.backlog` (versions) and `fs.replication.backlog.size` (bytes) | from the outbox tally: what a site loss would lose |
| `fs.replication.lag` (s) | age of the oldest pending version at the last pass: the RPO an operator actually has |
| `fs.replication.shipped` (versions) and `fs.replication.failed` (versions) | delivery outcomes |
| `fs.replication.blocks.offered` and `fs.replication.blocks.sent` | dedupe ratio; bytes sent as `fs.replication.sent` (By) |
| `fs.replication.backfill.remaining` | versions the walk has not reached |

On the destination, `fs.replication.applied` and `fs.replication.refused`
count versions received since start.

Logs:

- the runner logs each target's first failure after a success, and each
  recovery, instead of every retry;
- a refused version is logged with its key and reason.

## Admin API

- `GET`, `PUT` and `DELETE /api/v1/buckets/{bucket}/replication`: the
  targets, without their secrets. `PUT` validates as above.
- `GET /api/v1/buckets/{bucket}/replication/status`, per target:
  - backlog and lag, read from the outbox by any node;
  - the recent refused versions with their reasons, likewise;
  - the last success and the last error, and what was shipped since
    start, from the runner's memory: only the node running the bucket
    answers them (`runner: true`);
  - backfill progress: when it was asked for, versions walked and queued,
    an estimate of what is left, and whether the walk is done.
- A target given without its secret keeps the current one of that name, so
  a target can be edited without the secret at hand.
- `POST /api/v1/buckets/{bucket}/replication/backfill`: start or restart
  the walk.
- CLI: `fs replication set|get|rm|status|backfill`.

## S3 API

`?replication` (Put/GetBucketReplication) stays `NotImplemented`. AWS's
grammar names an IAM role and an ARN for the destination, and fs has no
place in it for the destination's endpoint and credentials. Pretending to
accept it would store a rule that cannot run. Clients see replication
through `x-amz-replication-status`.

## Testing

- **meta:** the outbox merge laws, property-tested like the other rows.
- **engine:**
  - every change listed above writes its outbox row in the same
    transaction, and the excluded ones do not;
  - applying a version is idempotent, order-independent, and never revives
    a `Gone` version;
  - an attrs-only change never creates a version;
  - SSE-S3 is rewrapped and SSE-C kept;
  - blocks are re-placed by the destination's scheme.
- **`internal/replication`:** a runner between two engines in one process:
  - delivery;
  - dedupe (a copy sends no blocks);
  - a destination down, then back, with backoff and catch-up;
  - a refused version that does not block the others;
  - backfill resuming after a restart;
  - two-way replication with no loop.
- **Integration:** two servers. Write through aws-sdk-go-v2 and minio-go,
  then check the versions, IDs, metadata, tags, the status headers and the
  ciphertext on the replica.
- **Chaos:** a second three-node cluster as a target of the soak's
  versioned bucket, reached through a TCP proxy that degrades the link a
  different way each round (40 ms per chunk, 1 MiB/s, random resets, an
  outage, a target node killed) while the source's own rounds kill, freeze
  and re-lay-out its nodes, moving the runner. Versions are deleted at the
  target by ID during the load. After the last round and a drain:
  - every version the source still holds exists at the destination with
    its ETag, unless deleted there (the source's lifecycle removes versions
    the target keeps, so the comparison is from the source's side);
  - nothing deleted at the destination by version came back;
  - every source version reports `x-amz-replication-status: COMPLETED`;
  - the runner logged at least one failure, so the faults reached it.

  Delete markers are not compared: the source's lifecycle creates and
  removes markers it does not replicate.

## Phasing

Each phase is a PR that leaves main working:

1. **Outbox** (done): the table, capture on every listed change, the
   lifecycle exclusion, compaction, the backlog metrics.
2. **Destination** (done): `?x-fs-replica` and the `admin` action,
   `fs.ReplicaReceiver` in the engine, block claims and intake, the replica
   flag, export, and `internal/replication.Client`.
3. **Runner** (done): the `replication` setting, the admin API and CLI,
   shipping, status, metrics, and the integration tests.
4. **Backfill** (done).
5. **`x-amz-replication-status`** (done).
6. **The chaos run** (done, `scripts/chaos/replication.go`): a second
   three-node cluster as the target of the
   soak's versioned buckets, behind a fault-injecting proxy (added delay,
   limited bandwidth, dropped and reset connections, outages), so the
   checks under Testing hold on every replication PR.
7. **A WAN soak on Hetzner:** two short-lived clusters in different
   locations, created and destroyed by a script (`hcloud`), running the
   chaos workload at fs's defaults for a few hours. It finds what a local
   proxy only approximates — real latency, bandwidth and TLS — and tunes the
   runner's batch, concurrency and timeouts against it. Run by hand or
   nightly, not per PR, since it costs money and its timing varies.

   The harness is `scripts/chaos` with `-serve-target` on one machine and
   `-target` on the other (see AGENTS.md). Its first run (Helsinki to
   Ashburn, about 100 ms, at fs's defaults, 20 rounds in 50 minutes) passed
   every check and drained in 2m9s, but under load the runner fell behind:
   lag reached 846 s and the backlog 4,000–5,500 versions (2–3.4 GB), worst
   in the rounds without injected faults. Versions ship one at a time, each
   paying at least two round trips (claim, then blocks), so a 100 ms link
   caps the runner near 5 versions a second; the default HTTP transport also
   keeps two idle connections per host, fewer than the blocks in flight, so
   the rest redo the TLS handshake.

## Open questions

- **Tag filters.** AWS rules filter by prefix and tags. Prefix covers the
  stated need; tags can follow the lifecycle filter code if asked for.
- **Replication time control.** AWS sells a 15-minute SLA with its own
  metrics. The lag metric gives an operator the same alert without a
  promise fs cannot make.
- **Deletes of specific versions.** AWS never replicates them, and neither
  does this design. A "mirror" mode that does is easy to add with this
  protocol (ship `Gone`), and dangerous. It waits for a request.

## Not in this design

- **Synchronous or multi-site-quorum writes.** That is a cluster stretched
  over zones, which the layout already supports.
- **Replication to non-fs endpoints**, which is out of scope.
- **Replicating buckets, settings, credentials or quotas.** A target bucket
  is created and configured by its operator, and the handshake checks what
  replication needs.
- **Multipart uploads in flight.** Only completed versions replicate, as in
  AWS.
