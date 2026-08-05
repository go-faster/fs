// Package metastore is the metadata plane's interface: everything the cluster
// knows about the object set apart from the sidecars that are its commit point.
//
// It exists to make that plane pluggable. The vocabulary here — an entry, a
// bucket's usage, a verification stamp, scrub coverage — is what every question
// about the object set is asked in, and it is the same whether the answer comes
// from one node's pebble database, from a cluster-wide store, or from a scan of
// the disks themselves. Naming it apart from any one engine is what lets a
// second engine arrive without touching a caller.
//
// # The target implementation
//
// Pluggability is the means, not the goal, and the interface is not neutral
// about where it is going. The destination is **sharded pebble running in the
// fs processes themselves** — the (bucket, key) space cut into ranges, each
// with one owner and log-shipped followers, the range map in etcd, the whole
// thing embedded in the storage nodes. That is a first-class citizen of this
// design rather than one option among several, because it is the only
// cluster-scope answer that keeps the project's single-external-dependency
// guarantee: no database to stand up, no second deployment role, and metadata
// capacity that grows with the cluster instead of being provisioned.
//
// The other implementations are deliberately not that. The node-local pebble
// store is today's code and stays the default for small clusters. The in-memory
// store is scaffolding: it makes the cluster-scope paths testable before the
// sharded plane exists, so that plane implements a settled interface instead of
// co-designing one, and it is not a deployment option.
//
// So when a signature here looks like it is accommodating something, check what
// it costs the sharded plane first — that is the implementation this interface
// is shaped for.
//
// The store is **derived, never authoritative**, and that is load-bearing
// rather than incidental. Sidecars remain the commit point: they are
// self-describing and sit next to the data, so a disk stays interpretable on
// its own and repair needs no external state. Losing a store costs a rebuild
// from those same disks and nothing else — which is what keeps every
// implementation of Store a cache rather than a database of record, and is why
// none of them needs consensus to be correct.
package metastore

import (
	"context"
	"time"

	"github.com/go-faster/fs/internal/cluster"
)

// Scope is how much of the cluster a Store describes.
//
// It is a capability rather than a configuration flag: a caller asks a store
// what it covers and picks its algorithm from the answer, instead of being told
// separately and risking the two disagreeing.
type Scope uint8

const (
	// ScopeLocal means the store describes only what this node holds, so a
	// cluster-wide answer is a merge across nodes.
	ScopeLocal Scope = iota
	// ScopeCluster means the store describes the whole cluster, so a
	// cluster-wide answer is one scan.
	ScopeCluster
)

// State is what a store believes about itself.
type State uint8

const (
	// StateBuilding means the store is not usable: it was never built, or the
	// last process did not close it and anything written since its last flush
	// may be missing. Either way a full rebuild is owed.
	StateBuilding State = iota
	// StateReady means a build completed and every process since handed the
	// store over cleanly.
	StateReady
)

// BuildCause is why a store is not ready.
//
// It exists because the two ways a store ends up unbuilt want different
// answers, and nothing could tell them apart. A plane that has never been built
// is waiting for a decision an operator makes — they turned it on, they can pick
// the window for the walk of every disk that follows. A plane that lost a range
// to a failure is degraded *now*, and every listing in the cluster is paying the
// slow path until it is rebuilt; waiting for a human there is waiting for
// nothing.
type BuildCause uint8

const (
	// CauseUnspecified is a store that is building with nothing recorded about
	// why — an older cluster, an unreadable flag, or a rebuild someone started
	// deliberately.
	//
	// It is the zero value and it is the cautious one. A rebuild is hours of
	// I/O competing with serving traffic, so a cause nobody can vouch for is not
	// grounds to start one.
	CauseUnspecified BuildCause = iota
	// CauseNeverBuilt is a store no build has ever completed: the plane was
	// switched on over a cluster that already held objects, so it describes only
	// what has been written since.
	CauseNeverBuilt
	// CauseOrphaned is a store that was ready and is not any more, because a
	// failure left a range with no copy of its data.
	CauseOrphaned
)

// String names the cause, for the flag an operator reads and for logs.
func (c BuildCause) String() string {
	switch c {
	case CauseNeverBuilt:
		return "never-built"
	case CauseOrphaned:
		return "orphaned"
	case CauseUnspecified:
		return "unspecified"
	default:
		return "unspecified"
	}
}

// Build is what a store believes about itself: whether it is usable, and when it
// is not, why.
type Build struct {
	State State
	// Cause is meaningful only while State is StateBuilding. A ready store has
	// no reason to be unready.
	Cause BuildCause
}

// Ready is the built state.
func Ready() Build { return Build{State: StateReady} }

// Building is the unbuilt state, with why.
func Building(cause BuildCause) Build {
	return Build{State: StateBuilding, Cause: cause}
}

// Entry is one object as the store holds it.
type Entry struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
	Size   int64  `json:"size"`
	ETag   string `json:"etag,omitempty"`
	// Modified, Seq and Generation carry the sidecar's ordering stamps, so the
	// store can tell a newer record from an older one arriving late — a
	// rebalance copying a superseded generation, say — and refuse to go
	// backwards.
	Modified   time.Time `json:"modified"`
	Seq        int64     `json:"seq,omitempty"`
	Generation string    `json:"generation,omitempty"`
	// Disk is where the newest record was seen. A node can hold copies of one
	// object on two disks under different epochs; the store keeps one entry
	// per object, naming the disk the winning record came from.
	Disk cluster.DiskID `json:"disk,omitempty"`
	// OwnerID and OwnerName are the principal recorded on the object. A V1
	// listing renders them, so a store that dropped them would quietly change
	// what an S3 client sees.
	OwnerID   string `json:"owner_id,omitempty"`
	OwnerName string `json:"owner_name,omitempty"`
	// VerifiedAt is when the scrub last checked this object's payload. Zero
	// means never, which is how a sweep finds what to do first.
	VerifiedAt time.Time `json:"verified_at,omitzero"`
	// Locator says where the object's bytes are when they are not a set of
	// per-object fragments. Zero — the only representation of absent — means
	// the file-per-object layout, which is every object today and the default
	// for buckets that never opt into aggregated volumes.
	//
	// Nothing reads or writes it yet. It exists now so that adding it is not a
	// migration later: it is a field on a pre-production format today and a
	// schema change across 10¹¹ rows once the metadata plane holds them. See
	// Locator.
	Locator Locator `json:"locator,omitzero"`
}

// Locator addresses a record inside an aggregated volume: which volume, where
// in it, and how long.
//
// It is the shape Haystack's index record has had since 2010 — (key, offset,
// size) against a named volume — and it is deliberately not the shape Ambry
// uses. Ambry embeds the partition id inside the blob id, so a get needs no
// lookup at all; that option is closed to an S3 store, whose keys are
// caller-chosen and must be enumerable in order. Carrying the locator as a
// field is the price of being an object store rather than a blob store.
//
// The widths are chosen so none of them has to be revisited:
//
//   - Volume is 64-bit because it is free and permanent. The exabyte target is
//     ~10⁷ volumes; 32 bits would do and would then be a format change the
//     first time someone shrank the volume size.
//   - Offset is 64-bit because a 100 GiB volume needs 37 of them, so 32 is not
//     merely tight, it is wrong.
//   - Length is 32-bit because a record is bounded by the inline threshold
//     (4 MiB, 22 bits); an object above it is never inside a volume.
type Locator struct {
	// Volume is the volume holding the record. Volume IDs are allocated from
	// 1, so zero means "not in a volume" and no separate presence flag is
	// needed in any backend — which also makes "round-trips as zero" and "is
	// absent" the same test.
	Volume uint64 `json:"volume,omitempty"`
	// Offset is where the record starts in the volume.
	Offset uint64 `json:"offset,omitempty"`
	// Length is the record's payload length.
	Length uint32 `json:"length,omitempty"`
}

// InVolume reports whether the entry's bytes live inside an aggregated volume
// rather than in per-object fragments.
func (l Locator) InVolume() bool { return l.Volume != 0 }

// Supersedes reports whether e is newer than other, by the same total order
// the sidecars use: sequence first, then write time, then the generation
// stamp as a deterministic tie-break.
//
// It lives on the entry rather than inside an implementation because every
// implementation owes the same answer: two stores that ordered late-arriving
// records differently would converge on different objects from the same disks.
func (e Entry) Supersedes(other Entry) bool {
	if e.Seq != other.Seq {
		return e.Seq > other.Seq
	}

	if !e.Modified.Equal(other.Modified) {
		return e.Modified.After(other.Modified)
	}

	return e.Generation > other.Generation
}

// Usage is what a bucket holds within the store's scope.
type Usage struct {
	Objects int64 `json:"objects"`
	Bytes   int64 `json:"bytes"`
}

// Verification is one object's verification stamp.
type Verification struct {
	Bucket string
	Key    string
	At     time.Time
}

// Coverage is how well the scrub is keeping up with what the store's scope
// holds.
type Coverage struct {
	// Objects is how many are held.
	Objects int64
	// Never is how many have not been verified once. A node that has never
	// completed a cycle reports them all.
	Never int64
	// Oldest is the least recent verification among those that have one. With
	// Never at zero it is the honest age of coverage: every object has been
	// checked at least since then.
	Oldest time.Time
}

// Store is the metadata plane.
//
// Every method takes a context and is expected to honor it — Scan and the
// other walks abort part-way rather than running to completion and discarding
// the result. A context that is accepted and ignored is worse than none at all,
// because it makes a later timeout bug invisible at exactly the call sites that
// were written trusting it.
//
// Close is the exception: teardown is not cancellable, and a Close that gave up
// half way would leave behind precisely the unflushed state the caller asked it
// to flush.
type Store interface {
	// Scope reports how much of the cluster this store describes. It is fixed
	// for the lifetime of the store.
	Scope() Scope

	// Put records an object, adjusting the bucket's counters by what it
	// displaces. A record that does not supersede the stored one is dropped.
	Put(ctx context.Context, e Entry) error
	// Delete removes an object and takes its bytes back out of the bucket's
	// counters. Removing what is not there is not an error.
	Delete(ctx context.Context, bucket, key string) error
	// Get returns one object's entry.
	Get(ctx context.Context, bucket, key string) (Entry, bool, error)

	// Usage returns a bucket's counters.
	Usage(ctx context.Context, bucket string) (Usage, error)
	// Buckets calls fn for every bucket the store holds counters for, in name
	// order, stopping when fn returns an error.
	//
	// It streams rather than returning a slice for the same reason Scan does:
	// nothing in S3 bounds how many buckets an account holds, and a
	// bucket-per-tenant deployment is an ordinary shape. A caller that wants
	// them all can still collect them — and the two that do are better off
	// deciding that explicitly, because materializing before the expensive
	// part is what keeps a backend from holding an iterator open across a
	// whole sweep.
	Buckets(ctx context.Context, fn func(bucket string) error) error

	// Scan calls fn for each of a bucket's objects whose key starts with prefix
	// and sorts after `after`, in key order, stopping after limit entries or
	// when fn returns an error. A limit of zero means every match.
	Scan(ctx context.Context, bucket, prefix, after string, limit int, fn func(Entry) error) error

	// SetVerified records when the scrub last checked these objects. Objects
	// the store does not hold are skipped rather than created.
	SetVerified(ctx context.Context, records []Verification) error
	// Coverage reports how stale verification is across the store's scope.
	Coverage(ctx context.Context) (Coverage, error)

	// State reports whether the store can be trusted.
	State(ctx context.Context) (State, error)
	// MarkReady records that a build finished.
	MarkReady(ctx context.Context) error
	// MarkBuilding records that the store is being rebuilt and must not be
	// trusted until it is not.
	MarkBuilding(ctx context.Context) error
	// Reset empties the store, for a rebuild that must not inherit stale
	// entries.
	Reset(ctx context.Context) error

	// Close releases the store. A store that is not closed is not damaged — it
	// is rebuilt.
	Close() error
}
