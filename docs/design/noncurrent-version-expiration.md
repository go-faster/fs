# Design: noncurrent-version expiration

**Status:** implemented. Required by [COMPATIBILITY.md](../../COMPATIBILITY.md)
("Planned: each requires a design document before commitment").

**Scope:** the lifecycle actions that bound a versioned bucket's growth:

- `NoncurrentVersionExpiration` with `NoncurrentDays` and
  `NewerNoncurrentVersions`;
- `Expiration` with `ExpiredObjectDeleteMarker`.

Transitions (`Transition`, `NoncurrentVersionTransition`) and tag and size
filters stay out of scope and keep failing with `NotImplemented`.

## Problem

In a versioned bucket nothing removes data by itself:

- An overwrite keeps the old version.
- A delete adds a delete marker and keeps every version under it.
- The lifecycle `Expiration` the server already enforces only makes the
  *current* version noncurrent: it writes a delete marker, as S3 does.

So a versioned bucket only grows. Its versions keep their blocks, and their
quota usage, until a client removes each one by ID. Without a way to bound
that, versioning is a trap.

There is also a cost specific to this engine. All versions of a key live in
one `objects` row (`meta.Object.Versions`), and every write to the key
rewrites the whole row. A frequently overwritten key in a versioned bucket
therefore gets slower to write and read the more versions it collects.
Removed versions turn into `Gone` entries inside the row, and tombstone
collection compacts those away a day later. Noncurrent expiration is what
bounds the row.

## S3 semantics

What clients rely on, from the S3 lifecycle documentation ("Lifecycle
configuration elements", "Examples of S3 Lifecycle configurations" and the
`NoncurrentVersionExpiration` API reference). Where the documentation is
silent, "Resolved questions" below records what fs chose.

**`NoncurrentVersionExpiration`**
- **`NoncurrentDays` (N ≥ 1):** a noncurrent version is removed permanently
  N days after it *became noncurrent*. That is when its successor (the next
  newer version or delete marker of the key) was created, not when the
  version itself was written. Like `Expiration.Days`, the instant is rounded
  up to the following UTC midnight.
- **`NewerNoncurrentVersions` (1–100, optional):** the N newest noncurrent
  versions are kept whatever their age. Only older ones are removed, and only
  once `NoncurrentDays` has also passed. A rule that sets it must have a
  `<Filter>` element (the legacy rule-level `<Prefix>` will not do); without
  one S3 answers `InvalidRequest`.
- `NoncurrentDays` is required: `NewerNoncurrentVersions` only adds a
  condition to it ("both values must be exceeded"). Ceph RGW 20.2 refuses
  `NewerNoncurrentVersions` alone, and an empty element, with `MalformedXML`.
- **Delete markers:** a noncurrent delete marker is a noncurrent version. The
  documentation's example of expiring "noncurrent objects that have no data"
  expires "noncurrent delete marker objects" with this action, so it also
  counts toward `NewerNoncurrentVersions`.

**`Expiration.ExpiredObjectDeleteMarker: true`**
- Removes a delete marker that is the key's current version *and* its only
  version: an "expired object delete marker". Such a marker hides nothing,
  but still takes a row and shows up in version listings.
- It may not appear with `Days` or `Date` in the same `Expiration` element.
  The documentation names no error; Ceph RGW 20.2 answers `MalformedXML`
  (for `Days` and for `Date`), as it and fs do for `Days` with `Date`.
- With `Expiration.Days` on a versioned bucket, S3 also removes expired object
  delete markers on its own, "when the delete markers are old enough to
  satisfy the age criteria". The sweep does the same: under such a rule, an
  expired object delete marker older than `Days` is removed.

**Suspended versioning:** a noncurrent version may be a `null` version (an
object written before versioning was enabled), and the same rules apply to
it. The `null` version a suspended-bucket write replaces is already removed
outright by the engine (`releaseReplaced`), so it never becomes noncurrent.

## Design

### The API (`fs.LifecycleRule`)

Three fields are added to `fs.LifecycleRule`:

```go
// NoncurrentDays removes a noncurrent version this many days after it
// became noncurrent. Zero means the rule removes no noncurrent version.
NoncurrentDays int
// NewerNoncurrentVersions keeps this many of a key's newest noncurrent
// versions whatever their age. Zero keeps none.
NewerNoncurrentVersions int
// ExpiredObjectDeleteMarker removes a key's delete marker once it is the
// key's only version.
ExpiredObjectDeleteMarker bool
```

`fs.LifecycleNoncurrentExpiry(rules, key, noncurrentSince, newer)` decides a
noncurrent version from:

- `noncurrentSince`: when it became noncurrent;
- `newer`: how many noncurrent versions of the key are newer than it.

When several rules match, the earliest removal wins, as in `LifecycleExpiry`.
S3 has no rule precedence, and keeping data a rule says should be gone is the
worse error.

### The S3 layer (`handler/bucket_lifecycle.go`)

**Parsing.** `NoncurrentVersionExpiration` and
`Expiration.ExpiredObjectDeleteMarker` leave `unsupportedLifecycleElement`, and
are parsed and validated:

- `NoncurrentDays` must be ≥ 1, and `NewerNoncurrentVersions` in 1–100. Out
  of range is `InvalidArgument` (RGW 20.2 agrees for `NoncurrentDays` 0; it
  does not range-check `NewerNoncurrentVersions` at all). A
  `NoncurrentVersionExpiration` without `NoncurrentDays` is `MalformedXML`.
- `NewerNoncurrentVersions` in a rule without a `<Filter>` element is
  `InvalidRequest`, as AWS documents. RGW 20.2 accepts it with the legacy
  `<Prefix>`; fs follows AWS.
- `ExpiredObjectDeleteMarker` together with `Days` or `Date` is
  `MalformedXML`.
- A rule with no action at all is refused, as today.
- `GET ?lifecycle` returns the new elements as they were sent.

**Versioning state.** Rules are accepted on a bucket in any versioning state,
as in S3. On a bucket that was never versioned they have nothing to act on.

**Capability check.** Rules are refused (`NotImplemented`) only when the
backend cannot list or remove versions, which is `storagemem` today. The
existing rule applies: a rule nothing enforces must not be stored.

### The sweep (`internal/lifecycle`)

The sweep stays written against `fs.Storage`. For a bucket whose backend
implements `fs.Versioner` and whose versioning has ever been set, one pass
walks `ListObjectVersions` (pages of 1000) instead of `ListObjects`. It
replaces the current walk rather than adding a second one, so a pass lists
the bucket once.

Versions arrive newest first within each key, so the sweep keeps a small
amount of state per key, carried across page boundaries:

- the previous entry's `LastModified`, which is when the current entry became
  noncurrent;
- how many noncurrent versions it has seen so far;
- whether the key's current version is a delete marker, and whether anything
  followed it.

For each entry:

1. **Current version, not a delete marker:** the existing `Expiration`
   logic, unchanged. The delete stays fenced on the listed ETag
   (`DeleteObjectIf`), so it writes a delete marker only if the object is
   still what the listing saw.
2. **Noncurrent version or delete marker:** if
   `LifecycleNoncurrentExpiry(rules, key, successor.LastModified, newer)` has
   passed, remove it with `DeleteObjectVersion(bucket, key, id)`. A version ID
   names immutable content, so this delete needs no fence: a write racing the
   sweep cannot be the version being removed.
3. **Current delete marker:** decided once the key's next entry, or the end
   of the listing, shows whether it was the only version. If it was, and a
   rule with `ExpiredObjectDeleteMarker` matches, or a rule with
   `Expiration.Days` whose age the marker has passed, remove it by its
   version ID. If a write lands in between, the marker is noncurrent by the time it's
   removed; removing a noncurrent marker changes nothing a reader sees.

Removing noncurrent versions first and then the marker within one pass needs
care. The listing still shows the versions that pass removed, so the sweep
counts a marker as "only version" if every entry after it was removed by this
pass. S3 needs a second lifecycle pass for this, and doing it in one costs
nothing.

**Known race.** `NewerNoncurrentVersions` is counted from the listing. Writes
racing the sweep only add newer versions, which makes keeping more the
mistake. But a client that deletes newer noncurrent versions by ID while the
sweep runs can leave fewer than N kept. S3's own lifecycle is asynchronous in
the same way. This is documented, not fenced.

**Ownership and cadence are unchanged:**
- In a cluster, one elected node sweeps each bucket (`sweepsBucket`).
- `lifecycle.interval` defaults to 12h, and passes are recorded in
  `.lastrun`.

**No new storage.** Everything goes through `fs.Versioner`, and removed
versions release their blocks through the engine's ordinary path. Since
#359, that also names them as GC candidates.

### Cost

A pass of a versioned bucket lists every version, where it used to list only
current objects. That's O(versions) per bucket every 12h. It's the price of
enforcement, and it's bounded by what the rules keep.

Removing a version is one `objects` row write (quorum) plus the release of
its block references, the same as a client's `DeleteObjectVersion`. A first
pass over a large backlog does that once per version. It's paced by the
sweep's single goroutine per bucket, and it can't starve client writes to
the same key, because it takes the key's ordinary write lock.

A batch removal capability, taking several versions of one key in one row
write, would cut that write amplification. It's deferred until a backlog
shows it's needed. The interface would be an optional `fs.VersionPruner`,
like the other optional capabilities.

### Observability

The lifecycle sweep exports no metrics today, only a log line per pass, and
this change fixes that. A new `fs.lifecycle.*` set, registered in `cmd/fs`:

- `fs.lifecycle.age`: seconds since the last completed pass, the number that
  goes wrong first;
- `fs.lifecycle.removed{action}`, counted since start:
  - `expired`: current versions;
  - `noncurrent`: noncurrent versions;
  - `marker`: expired delete markers;
  - `aborted`: abandoned uploads;
- `fs.lifecycle.raced`: removals skipped because the object changed;
- `fs.lifecycle.failed_buckets`: buckets the last pass couldn't finish.

`Report` gains the same counters, so the per-pass log line carries them too.

**Admin API:** none needed. Rules are an S3-API surface (`?lifecycle`), and
there's no engine-stats endpoint to extend. If one is added later, it should
carry the lifecycle numbers.

## Testing

s3-tests can't gate this: the `s3t` port carries no lifecycle tests, and the
upstream ones depend on Ceph's lifecycle debug clock. Coverage comes from:

- **Unit tests (`fs`):** `LifecycleNoncurrentExpiry`. Midnight rounding, the
  `NewerNoncurrentVersions` boundary (keep exactly N), and several rules
  where the earliest wins.
- **Sweep tests (`internal/lifecycle`), against the engine, with `Now` moved
  forward:**
  - noncurrent versions removed at the right day, counting from when each
    was replaced;
  - N newest noncurrent kept;
  - an expired delete marker removed, in the same pass as its last
    noncurrent version;
  - a marker with versions under it kept;
  - a key spanning a page boundary;
  - suspended versioning with a `null` noncurrent version;
  - a version removed by a client mid-pass;
  - quota usage and block references released, so GC collects the blocks.
- **Handler tests:** parse, validate and round-trip each new element; the
  refusals; and `NotImplemented` on a backend without `fs.Versioner`.
- **Integration (`integration/`, minio-go and aws-sdk-go-v2):**
  `PutBucketLifecycleConfiguration` with each element, `Get` round-trip, and
  one end-to-end expiry through a server whose sweep runs on a test clock.
- **Docs:** `COMPATIBILITY.md` (lifecycle row and the "Planned" list), and
  `docs/DEPLOYMENT.md` for the metrics.

## Resolved questions

Checked against the AWS documentation and, where it is silent, against Ceph
RGW 20.2.4 (tentacle):

1. **Noncurrent delete markers** count toward `NoncurrentVersionExpiration`
   and `NewerNoncurrentVersions`: the documentation expires them with this
   action.
2. **`ExpiredObjectDeleteMarker` with `Days` or `Date`** is documented as not
   allowed, without naming the error. RGW answers `MalformedXML` for both, so
   fs does too.
3. **`Expiration.Days` on a versioned bucket** removes expired object delete
   markers old enough to satisfy it, without `ExpiredObjectDeleteMarker`. The
   sweep does the same.
4. **`NewerNoncurrentVersions` without `NoncurrentDays`** is refused with
   `MalformedXML`, as RGW does. The API reference marks both optional, but
   the user guide makes it a condition on `NoncurrentDays`.

Found while checking: `NewerNoncurrentVersions` needs a `<Filter>` element,
or the rule is `InvalidRequest`.

All four, and the `<Filter>` requirement, were later confirmed against AWS
S3 itself on 2026-10-26, with the same error codes. One case differed and
was fixed: AWS accepts a rule whose only action is
`ExpiredObjectDeleteMarker` set to `false`, which fs had refused.

## Not in this design

- Transitions and storage classes: there's one storage class.
- Tag and size filters (`Filter.Tag`, `And`, `ObjectSizeGreaterThan`): a
  separate change. The sweep would need each version's tags.
- Batch version removal (`fs.VersionPruner`): see Cost.
