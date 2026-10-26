# Design: lifecycle tag and size filters

**Status:** proposed. Required by [COMPATIBILITY.md](../../COMPATIBILITY.md)
("Planned: each requires a design document before commitment").

**Scope:** the rest of the lifecycle `<Filter>` grammar:

- `<Tag>`: objects carrying one tag;
- `<ObjectSizeGreaterThan>` and `<ObjectSizeLessThan>`: objects by size;
- `<And>`: a prefix, any number of tags and the size bounds together.

They apply to every action the sweep enforces: `Expiration`,
`NoncurrentVersionExpiration` and `ExpiredObjectDeleteMarker`, plus
`AbortIncompleteMultipartUpload` where S3 allows it. Transitions stay out of
scope and keep failing with `NotImplemented`.

## Problem

A rule can only select objects by key prefix today. Every other filter
element is refused with `NotImplemented` (`unsupportedLifecycleElement`).
That rules out the two common ways clients mark data as disposable without
reorganizing their keys:

- **By tag:** "expire everything tagged `retention=7d`", where the writer
  decides an object's lifetime when it uploads it. This is the usual pattern
  for backups, CI artifacts and logs that share a prefix with data that must
  stay.
- **By size:** "expire objects under 1 KiB after a day" (empty markers,
  heartbeats), or "keep only the large objects' history short".

A client with such a configuration gets `NotImplemented` and has to fall back
to deleting objects itself.

## S3 semantics

From the S3 lifecycle documentation ("Lifecycle configuration elements",
"Filtering objects by size") and the `LifecycleRuleFilter` and
`LifecycleRuleAndOperator` API references. Where the documentation is silent,
Ceph RGW 20.2.4 (tentacle) was asked; "Validation" below records each case
and what fs does.

**Filter shape.** A `<Filter>` holds exactly one of `Prefix`, `Tag`,
`ObjectSizeGreaterThan`, `ObjectSizeLessThan` or `And`. To combine
predicates, they go inside `<And>`, which holds a `Prefix`, any number of
`Tag` elements and the two size bounds. An object matches when it satisfies
every predicate. An empty `<Filter/>` matches the whole bucket, as today.

**Tags.** An object matches a `<Tag>` when it carries a tag with that key
*and* that value. The tags are the ones the version carries when the sweep
looks, not when it was written: `PutObjectTagging` after the upload moves the
object into or out of a rule. Retagging does not change `LastModified`, so
the expiry is still counted from the write. Each version has its own tags, so
a noncurrent version is judged by its own.

**Sizes.** Both bounds are strict and in bytes: `ObjectSizeGreaterThan` N
matches objects of more than N bytes, and `ObjectSizeLessThan` N those of less
than N. The size is the object's size as written by the client, the one
listings report.

**Restrictions:**

- `ExpiredObjectDeleteMarker` cannot be used with a tag filter: delete
  markers have no tags.
- `AbortIncompleteMultipartUpload` cannot be used with a tag filter:
  uploads in progress have no tags yet.
- When both size bounds are set, `ObjectSizeGreaterThan` must be less than
  `ObjectSizeLessThan`.

**Delete markers** carry no tags, so a tag rule never matches one. That
includes removing an expired object delete marker under `Expiration.Days`.
For a size predicate, a delete marker counts as 0 bytes.

## Design

### The API (`fs.LifecycleRule`)

The rule's filter grows from a prefix to the full predicate set:

```go
// Tags restricts the rule to versions carrying every one of these tags,
// with these values. Empty matches any tags.
Tags []Tag
// SizeGreaterThan restricts the rule to versions of more than this many
// bytes. Nil sets no lower bound.
SizeGreaterThan *int64
// SizeLessThan restricts the rule to versions of fewer than this many
// bytes. Nil sets no upper bound.
SizeLessThan *int64
```

Pointers, because 0 is a meaningful bound: `ObjectSizeGreaterThan 0` excludes
empty objects. The rules are stored as JSON in a bucket setting, so the new
fields need no format change. Rules written before this change decode with
nil bounds and no tags, which keeps their meaning.

`Matches(key)` becomes `Matches(o LifecycleObject)`, where `LifecycleObject`
is what a rule can judge:

```go
type LifecycleObject struct {
	Key  string
	Size int64
	Tags []Tag
}
```

`LifecycleExpiry`, `LifecycleNoncurrentExpiry` and `LifecycleMarkerExpiry`
take a `LifecycleObject` instead of a key. A delete marker is passed with
size 0 and no tags. `HasTags()` and `HasSize()` report whether a rule uses
those predicates, for the sweep's listing and for validation.

Uploads in progress are matched by `MatchesUpload(key)`, which takes the
prefix only. Validation guarantees that a rule with an
`AbortIncompleteMultipartUpload` action has neither tags nor sizes.

### Listings carry tags (`fs.Storage`)

The sweep needs each version's tags. Asking for them one object at a time
would cost a quorum read per candidate on every pass. For a rule like "expire
`tmp=true` objects after a day" in a bucket of ten million old untagged
objects, that is ten million reads a pass to delete nothing.

The engine has no such cost. A version's tags live in its `Attrs`, inside the
same `objects` row the listing already reads. They only need decoding.

So `fs.Object` and `fs.ObjectVersion` gain `Tags []Tag`, filled when the
request asks for them:

```go
// ListObjectsRequest, ListObjectVersionsRequest:
// Tags asks for each entry's tags. Listings for S3 clients leave it off:
// S3 listings do not return tags, and decoding them is wasted work.
Tags bool
```

This changes the `fs.Storage` contract but not its method set. The work:

- **`engine`:** `ListObjects` and `listVersions` decode `Attrs` when asked.
- **`storagemem`:** copies the object's tags.
- **`storagetest`:** a conformance case that a listing with `Tags` returns
  each version's tags, and one without it returns none. Both backends inherit
  it.

### The S3 layer (`handler/bucket_lifecycle.go`)

**Parsing.** `LifecycleFilterXML` parses `Tag`, `ObjectSizeGreaterThan`,
`ObjectSizeLessThan` and `And` (with its `Prefix`, `Tag` list and size
bounds). They leave `unsupportedLifecycleElement`. The rule-level legacy
`<Prefix>` stays prefix-only, as in S3.

**Validation:**

| Case | AWS docs | RGW 20.2 | fs |
|------|----------|----------|----|
| More than one predicate directly under `<Filter>` | exactly one | accepts | `MalformedXML` |
| `<And>` with fewer than two predicates | "two or more" | accepts, renders bare | accepts, renders bare |
| `ObjectSizeGreaterThan` ≥ `ObjectSizeLessThan` | not allowed | `MalformedXML` | `MalformedXML` |
| Negative size | silent | accepts | `InvalidArgument` |
| Tag filter with `ExpiredObjectDeleteMarker` | not allowed | `InvalidRequest` | `InvalidRequest` |
| Tag filter with `AbortIncompleteMultipartUpload` | not allowed | `InvalidRequest` | `InvalidRequest` |
| Size filter with `AbortIncompleteMultipartUpload` | silent | accepts | `InvalidRequest` |
| Size filter with `ExpiredObjectDeleteMarker` | silent | accepts | accepts; a marker is 0 bytes |
| Tag key empty or over 128 bytes, value over 256 | object-tag limits | not checked | `InvalidTag` |
| Two tags with the same key in `<And>` | silent | accepts | `InvalidTag` |
| `<Tag>` without `<Value>` | silent | accepts | accepts as the empty value |

The cases where fs is stricter than RGW:

- **Several bare predicates.** The documentation says "exactly one". An SDK
  sends whatever the caller builds, so the error is what tells a client its
  filter is ambiguous.
- **A size filter with an abort.** An upload in progress has no size, so the
  rule would never abort anything. A lifecycle action that is stored but can
  never run is what the project already refuses rather than keeping
  (`ErrUnsupportedOperation`). The same reasoning applies here.
- **Negative sizes and duplicate tag keys** describe rules that cannot match
  as written. `InvalidTag` and its limits come from `validateTags`, which
  object tagging already uses, minus the limit of ten tags per object, which
  is not a lifecycle rule.

**Rendering.** `GET ?lifecycle` returns a single predicate bare and several
inside `<And>`, as S3 and RGW do. An `<And>` that was sent with one predicate
comes back without the `<And>`, as in RGW. (RGW also places size bounds
outside `<And>` when tags are present. That is an RGW quirk, and fs keeps
them inside.)

**Capability.** Every backend can carry tags in a listing, so these rules are
accepted wherever lifecycle rules are. The versioned actions keep their
existing check.

### The sweep (`internal/lifecycle`)

- **Listing.** A pass lists a bucket with `Tags: true` only when one of its
  enabled rules `HasTags()`. A prefix- or size-only configuration pays
  nothing.
- **Unversioned buckets (`expireObjects`):** each listed `fs.Object` becomes
  a `LifecycleObject` with its own key, size and tags.
- **Versioned buckets (`expireVersion`):**
  - The current version and each noncurrent one are judged by their own
    size and tags.
  - `newer` still counts every noncurrent version of the key, whether or not
    it matches. `NewerNoncurrentVersions` is about how many newer versions
    the key keeps, not how many the rule would select.
  - A version a rule does not match is `left`, so the delete marker above it
    stays: something is still under it.
- **Delete markers:** passed as 0 bytes with no tags, so tag rules never
  touch them, and a size rule does only through `ObjectSizeLessThan`.
- **Fencing is unchanged.** The current version is still removed with an
  ETag-conditional delete, and noncurrent versions by ID.
  - Retagging between the listing and the removal changes neither the ETag
    nor the version ID. A version listed with a matching tag can therefore be
    removed just after its tag was changed away.
  - S3 makes no stronger promise ("rules are evaluated asynchronously"), and
    the window is one page of the walk.
  - Closing it would need tag-conditional deletes in the engine, which no
    client of S3 can ask for. It is not worth adding.

### Cost

- No new reads. Tags come from rows the walk already reads, and are decoded
  only for buckets with tag rules.
- Matching is a prefix test, two integer comparisons and a scan of a few
  tags per version.
- A bucket with tag rules allocates its tags once per listed version per
  pass. That is the same order as the rows the pass already decodes.

### Observability

- Nothing new to export. Removals still count under
  `fs.lifecycle.removed{action}`, by action rather than by filter; a filter
  is not a new way for the sweep to fall behind.
- The pass's log line and `fs.lifecycle.age` already show a sweep that stops
  completing.

### Admin API

Nothing: rules are set and read over the S3 API, as today.

## Testing

- **`lifecycle_test.go`:**
  - `Matches` over prefix, tag, both bounds and `And` combinations.
  - Strict bounds at N, N−1 and N+1.
  - Delete markers against tag and size rules.
  - The earliest removal when a tag rule and a prefix rule both match.
- **`handler/bucket_lifecycle_test.go`:** every row of the validation table,
  and round trips of a bare `Tag`, a bare size bound, an `And` with a prefix,
  two tags and both bounds, and an `And` with one predicate rendered bare.
- **`internal/lifecycle`, engine-backed:**
  - Tagged and untagged objects under one prefix, with only the tagged ones
    expired.
  - A tag added by `PutObjectTagging` after the write, which brings the
    object into the rule.
  - A noncurrent version judged by its own tags while the current one's
    differ.
  - A key whose unmatched noncurrent version keeps its delete marker.
  - Size bounds at the edges.
  - A configuration without tag rules listing with `Tags: false`, through a
    wrapper that records the request.
- **`storagetest`:** listings with and without `Tags`, on both backends.
- **`integration/`:** aws-sdk-go-v2 and minio-go set and read back an `And`
  filter with tags and sizes. The AWS case also sweeps on a moved clock and
  checks what `ListObjectVersions` shows.
- **s3-tests:** s3t ports none of the lifecycle tests, so no known-failures
  lines move. The integration tests are the client-facing evidence, as for
  #362.
- **Chaos:** the soak's lifecycle rules could carry a tag predicate. That is
  not needed: tags reach the sweep through the same rows the soak already
  exercises.

## Open questions

1. **Delete markers under a size filter.** The documentation does not say
   whether `ObjectSizeLessThan` covers delete markers. fs treats a marker as
   0 bytes: S3 lists markers with no size, and `fs.ObjectVersion` holds 0
   for one. The alternative
   is that size predicates never match markers, which would make
   `ExpiredObjectDeleteMarker` inert under any size filter. Checking needs a
   bucket on AWS itself; RGW's answer would only show RGW.
2. **Size filter with `AbortIncompleteMultipartUpload`.** fs refuses this
   combination where RGW accepts it. If AWS accepts it and aborts by some
   notion of an upload's size, refusing would break a client that works
   against AWS. The same check on AWS settles it.

## Not in this design

- Transitions and storage classes: there's one storage class.
- Object Lock: out of scope.
- Filtering by metadata or content type: S3 has no such predicate.
