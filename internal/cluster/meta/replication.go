package meta

import (
	"maps"
	"slices"

	"github.com/go-faster/fs/internal/cluster/table"
)

// Replication is the outbox of geo-replication: partition key the bucket's
// ID, sort key OutboxSK of the object key and version ID. A row records that
// a version changed and has to reach the bucket's replication targets, and
// how far each target has received it. It is written in the same commit as
// the change, so an acknowledged write always has its row.
const Replication = "replication"

// OutboxSK is the outbox sort key of one version of key.
func OutboxSK(key, versionID string) string { return key + "\x00" + versionID }

// Outbox is the outbox row of one version.
type Outbox struct {
	// Targets are the replication targets the version is for, by name.
	// Sorted.
	Targets []string `json:"targets"`
	// Pending is the TS of the newest change to ship.
	Pending int64 `json:"pending"`
	// Version is set when the version itself is to ship, not only a change
	// of its attributes. A row recreated after its collection by an
	// attributes change carries none: the version reached every target
	// before, and an attributes change must not bring back a version a
	// target has since deleted.
	Version bool `json:"version,omitempty"`
	// Size is the version's size, for the backlog's bytes.
	Size int64 `json:"size,omitempty"`
	// Shipped is, per target, the Pending a delivery last carried.
	Shipped map[string]int64 `json:"shipped,omitempty"`
}

// PendingFor reports whether the row still has to reach target.
func (o Outbox) PendingFor(target string) bool {
	_, ok := slices.BinarySearch(o.Targets, target)

	return ok && o.Shipped[target] < o.Pending
}

// Done reports whether every target has the row's newest change.
func (o Outbox) Done() bool {
	for _, t := range o.Targets {
		if o.Shipped[t] < o.Pending {
			return false
		}
	}

	return true
}

// MergeOutbox is the replication table's merge: the union of targets, and the
// greatest of every counter.
func MergeOutbox(a, b Outbox) Outbox {
	out := Outbox{
		Targets: slices.Compact(slices.Sorted(slices.Values(slices.Concat(a.Targets, b.Targets)))),
		Pending: max(a.Pending, b.Pending),
		Version: a.Version || b.Version,
		Size:    max(a.Size, b.Size),
	}

	if len(out.Targets) == 0 {
		out.Targets = nil
	}

	if len(a.Shipped)+len(b.Shipped) > 0 {
		out.Shipped = maps.Clone(a.Shipped)
		if out.Shipped == nil {
			out.Shipped = make(map[string]int64, len(b.Shipped))
		}

		for t, ts := range b.Shipped {
			out.Shipped[t] = max(out.Shipped[t], ts)
		}
	}

	return out
}

// CompactOutbox is the replication table's compaction: a row every target
// has received is done with.
func CompactOutbox(o Outbox) (Outbox, table.Compaction) {
	if o.Done() {
		return Outbox{}, table.Delete
	}

	return o, table.Keep
}
