package rangemap_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/rangemap"
)

// three is a hand-built map, so the lookup tests do not depend on whatever
// boundaries Initial happens to choose.
func three() *rangemap.Map {
	return &rangemap.Map{Ranges: []rangemap.Range{
		{Start: "", End: "om", Owner: "n0"},
		{Start: "om", End: "ot", Owner: "n1"},
		{Start: "ot", End: "", Owner: "n2"},
	}}
}

// TestLookupIsTotal: the ranges cover the key space, so every key has an owner.
// This is the property a gap would break, and it would break it silently —
// routing the key to the range before it, whose owner does not hold it, so the
// object reads as absent rather than as an error.
func TestLookupIsTotal(t *testing.T) {
	m := three()

	for _, key := range []string{
		"",         // the very start
		"\x00",     // before any object key
		"oa", "ol", // first range
		"om",        // exactly a boundary: belongs to the range it starts
		"omm", "os", // second range
		"ot",                // the next boundary
		"ozzz",              // last range
		"p", "\xff\xff\xff", // past every object key, still owned
	} {
		t.Run(fmt.Sprintf("%q", key), func(t *testing.T) {
			r, ok := m.Lookup(key)
			require.True(t, ok, "every key must have an owner")
			assert.True(t, r.Contains(key), "the range returned must actually contain the key")
		})
	}
}

// TestLookupBoundariesAreHalfOpen: a boundary key belongs to the range it
// starts, not the one it ends. Getting this wrong puts one key on the wrong
// node — invisible until that exact key is written.
func TestLookupBoundariesAreHalfOpen(t *testing.T) {
	m := three()

	owner, ok := m.Owner("om")
	require.True(t, ok)
	assert.EqualValues(t, "n1", owner, "a boundary starts its range")

	owner, ok = m.Owner("ol\xff")
	require.True(t, ok)
	assert.EqualValues(t, "n0", owner, "the key just below belongs to the previous range")
}

// TestLookupOnEmptyMap distinguishes "not initialized" from "unowned key". The
// second cannot happen in a valid map; the first is a cluster whose metadata
// plane has not been set up, and a caller should be able to tell.
func TestLookupOnEmptyMap(t *testing.T) {
	var m rangemap.Map

	_, ok := m.Lookup("anything")
	assert.False(t, ok)
}

func TestRangesFor(t *testing.T) {
	m := three()

	assert.Equal(t, []rangemap.Range{{Start: "om", End: "ot", Owner: "n1"}}, m.RangesFor("n1"))
	assert.Empty(t, m.RangesFor("n9"), "a node owning nothing owns nothing")
}

// TestValidateRejectsWhatWouldFailSilently is the point of Validate: none of
// these produce an error at lookup time, they produce a wrong answer.
func TestValidateRejectsWhatWouldFailSilently(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ranges []rangemap.Range
		want   string
	}{
		{
			name:   "empty",
			ranges: nil,
			want:   "empty",
		},
		{
			name: "a gap between ranges",
			ranges: []rangemap.Range{
				{Start: "", End: "om", Owner: "n0"},
				{Start: "op", End: "", Owner: "n1"},
			},
			want: "gap or overlap",
		},
		{
			name: "an overlap between ranges",
			ranges: []rangemap.Range{
				{Start: "", End: "ot", Owner: "n0"},
				{Start: "om", End: "", Owner: "n1"},
			},
			want: "gap or overlap",
		},
		{
			name: "not starting at the beginning",
			ranges: []rangemap.Range{
				{Start: "oa", End: "", Owner: "n0"},
			},
			want: "not the empty key",
		},
		{
			name: "not reaching the end",
			ranges: []rangemap.Range{
				{Start: "", End: "om", Owner: "n0"},
			},
			want: "not the end of the key space",
		},
		{
			name: "an unowned range",
			ranges: []rangemap.Range{
				{Start: "", End: "", Owner: ""},
			},
			want: "no owner",
		},
		{
			name: "an inverted range",
			ranges: []rangemap.Range{
				{Start: "", End: "ot", Owner: "n0"},
				{Start: "ot", End: "om", Owner: "n1"},
			},
			want: "empty or inverted",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &rangemap.Map{Ranges: tc.ranges}
			require.ErrorContains(t, m.Validate(), tc.want)
		})
	}
}

func TestValidateAcceptsAWholeMap(t *testing.T) {
	require.NoError(t, three().Validate())
}

// TestInitialPartitionsTheKeySpace: whatever the boundaries, the result has to
// be a partition — that is the invariant, and the boundaries are a guess.
func TestInitialPartitionsTheKeySpace(t *testing.T) {
	nodes := []cluster.NodeID{"n0", "n1", "n2"}

	for _, n := range []int{1, 2, 3, 7, 16, 64} {
		t.Run(fmt.Sprintf("%d ranges", n), func(t *testing.T) {
			m, err := rangemap.Initial(n, nodes)
			require.NoError(t, err)
			require.NoError(t, m.Validate())
			require.Len(t, m.Ranges, n)

			// Every key lands somewhere, including ones outside the band the
			// boundaries were chosen from.
			for _, key := range []string{"", "o", "oa", "obucket\x00key", "oz", "p", "\xff"} {
				_, ok := m.Lookup(key)
				assert.True(t, ok, "key %q must be owned", key)
			}
		})
	}
}

// TestInitialSpreadsAcrossNodes: a presplit that gave every range to one node
// would be a partition and still pointless.
func TestInitialSpreadsAcrossNodes(t *testing.T) {
	nodes := []cluster.NodeID{"n0", "n1", "n2"}

	m, err := rangemap.Initial(9, nodes)
	require.NoError(t, err)

	for _, node := range nodes {
		assert.Len(t, m.RangesFor(node), 3, "node %s", node)
	}
}

func TestInitialRefusesNonsense(t *testing.T) {
	_, err := rangemap.Initial(0, []cluster.NodeID{"n0"})
	require.ErrorContains(t, err, "at least 1")

	_, err = rangemap.Initial(4, nil)
	require.ErrorContains(t, err, "no nodes")
}

// TestInitialBoundariesAreOrdered guards the arithmetic in boundary(): a
// rounding mistake there produces duplicate or descending split points, which
// Validate catches as an inverted range — but only if someone runs it.
func TestInitialBoundariesAreOrdered(t *testing.T) {
	m, err := rangemap.Initial(32, []cluster.NodeID{"n0"})
	require.NoError(t, err)

	for i := 1; i < len(m.Ranges); i++ {
		assert.Less(t, m.Ranges[i-1].Start, m.Ranges[i].Start,
			"boundary %d must sort after its predecessor", i)
	}
}
