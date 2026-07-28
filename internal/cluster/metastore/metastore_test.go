package metastore_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/go-faster/fs/clusterstore"
	"github.com/go-faster/fs/internal/cluster/metastore"
)

var (
	t0 = time.Date(2026, 7, 27, 12, 0, 0, 0, time.UTC)
	t1 = t0.Add(time.Minute)
)

// ordering is one comparison, expressed so the same case can be run against
// both implementations of the rule.
type ordering struct {
	name string
	// a and b are (seq, modified, generation).
	aSeq, bSeq int64
	aMod, bMod time.Time
	aGen, bGen string
	// want is whether a supersedes b.
	want bool
}

// cases covers each tier of the order and the boundary between tiers: a lower
// tier decides only when every tier above it is equal.
var cases = []ordering{
	{"higher seq wins", 2, 1, t0, t0, "g", "g", true},
	{"lower seq loses", 1, 2, t0, t0, "g", "g", false},
	{"seq outranks a newer modified", 2, 1, t0, t1, "g", "g", true},
	{"seq outranks a higher generation", 2, 1, t0, t0, "a", "z", true},
	{"equal seq falls through to modified", 1, 1, t1, t0, "g", "g", true},
	{"equal seq, older modified loses", 1, 1, t0, t1, "g", "g", false},
	{"modified outranks generation", 1, 1, t1, t0, "a", "z", true},
	{"equal seq and modified falls through to generation", 1, 1, t0, t0, "z", "a", true},
	{"equal seq and modified, lower generation loses", 1, 1, t0, t0, "a", "z", false},
	{"identical records do not supersede each other", 1, 1, t0, t0, "g", "g", false},
	{"zero seq is ordered, not ignored", 0, 0, t1, t0, "g", "g", true},
}

func (o ordering) entries() (a, b metastore.Entry) {
	return metastore.Entry{Seq: o.aSeq, Modified: o.aMod, Generation: o.aGen},
		metastore.Entry{Seq: o.bSeq, Modified: o.bMod, Generation: o.bGen}
}

func (o ordering) sidecars() (a, b *clusterstore.Sidecar) {
	return &clusterstore.Sidecar{Seq: o.aSeq, Modified: o.aMod, Generation: o.aGen},
		&clusterstore.Sidecar{Seq: o.bSeq, Modified: o.bMod, Generation: o.bGen}
}

// TestSupersedes pins the total order itself: sequence first, then write time,
// then the generation stamp as a deterministic tie-break.
//
// It lives here rather than in a backend's tests because the rule belongs to
// the entry, not to whatever is storing it. Two backends that ordered
// late-arriving records differently would converge on different objects from
// the same disks — which is the one way a derived store can corrupt rather
// than merely lag.
func TestSupersedes(t *testing.T) {
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, b := tc.entries()
			assert.Equal(t, tc.want, a.Supersedes(b))
		})
	}
}

// TestSupersedesIsAntisymmetric: for any two records that are not equal under
// the order, exactly one supersedes the other. A rule that let both win would
// make the outcome depend on which record happened to arrive first, and a rule
// that let neither win would strand an object at whichever version an
// interrupted rebuild reached.
func TestSupersedesIsAntisymmetric(t *testing.T) {
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, b := tc.entries()

			if a == b {
				assert.False(t, a.Supersedes(b), "a record cannot supersede itself")
				return
			}

			assert.NotEqual(t, a.Supersedes(b), b.Supersedes(a),
				"exactly one of a>b and b>a must hold")
		})
	}
}

// TestSupersedesMatchesTheSidecar is the one that matters as backends multiply.
//
// Sidecars are the commit point and entries are derived from them, so the two
// rules are not merely similar — they have to be the same rule. If they drift,
// a rebuild from the disks picks a different winner than the write path did,
// and the index disagrees with the data it describes while both look internally
// consistent. Nothing else in the tree pins this, and it gets easier to break
// with every backend added.
func TestSupersedesMatchesTheSidecar(t *testing.T) {
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entryA, entryB := tc.entries()
			scA, scB := tc.sidecars()

			assert.Equal(t, entryA.Supersedes(entryB), scA.Supersedes(scB),
				"metastore.Entry and clusterstore.Sidecar must order records identically")
		})
	}
}

// TestModifiedComparesByInstant: Equal, not ==, so the same instant in two
// locations is one instant. Sidecars round-trip through JSON and come back with
// whatever zone the writer used, so a wall-clock comparison here would order
// two copies of one record differently depending on which node read them.
func TestModifiedComparesByInstant(t *testing.T) {
	utc := metastore.Entry{Seq: 1, Modified: t0, Generation: "g"}
	elsewhere := metastore.Entry{
		Seq:        1,
		Modified:   t0.In(time.FixedZone("somewhere", 3*60*60)),
		Generation: "g",
	}

	assert.False(t, utc.Supersedes(elsewhere))
	assert.False(t, elsewhere.Supersedes(utc))
}

// TestScopeDefaultsToLocal: the zero Scope is ScopeLocal, so a backend that
// forgets to declare one describes a node rather than claiming to describe the
// cluster. Getting this backwards would turn a missing declaration into a
// listing that silently returns one node's objects as if they were all of them.
func TestScopeDefaultsToLocal(t *testing.T) {
	var s metastore.Scope

	assert.Equal(t, metastore.ScopeLocal, s)
	assert.NotEqual(t, metastore.ScopeCluster, s)
}

// TestStateDefaultsToBuilding: the zero State is StateBuilding, so a store that
// has not said otherwise is not trusted. The safe default is the whole reason
// unsynced writes are affordable.
func TestStateDefaultsToBuilding(t *testing.T) {
	var s metastore.State

	assert.Equal(t, metastore.StateBuilding, s)
}
