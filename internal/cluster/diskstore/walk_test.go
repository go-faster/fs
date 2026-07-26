package diskstore_test

import (
	"context"
	"path/filepath"
	"sort"
	"testing"

	"github.com/go-faster/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/diskstore"
)

// walkAll collects a walk into a slice, which tests may do and the scrubber may
// not.
func walkAll(t *testing.T, s *diskstore.Store, after string) []string {
	t.Helper()

	var got []string

	require.NoError(t, s.WalkFragments(t.Context(), "d0", after, func(name string) error {
		got = append(got, name)

		return nil
	}))

	return got
}

// fragmentTree writes a handful of object namespaces shaped like the real
// thing: fixed-width hex directories, a sidecar and generation-stamped
// fragments in each.
func fragmentTree(t *testing.T, s *diskstore.Store) []string {
	t.Helper()

	bucket := "b7d3" + "00000000000000000000000000000000000000000000000000000000000"
	objects := []string{
		"0a11" + "00000000000000000000000000000000000000000000000000000000000",
		"3f22" + "00000000000000000000000000000000000000000000000000000000000",
		"c933" + "00000000000000000000000000000000000000000000000000000000000",
	}

	var want []string

	for _, obj := range objects {
		dir := "obj/" + bucket + "/" + obj

		for _, file := range []string{"meta", "aa11bb22.f0", "aa11bb22.f1"} {
			name := dir + "/" + file
			put(t, s, "d0", name, []byte("x"))

			want = append(want, name)
		}
	}

	sort.Strings(want)

	return want
}

func TestWalkFragmentsInOrder(t *testing.T) {
	s := newStore(t, "d0")
	want := fragmentTree(t, s)

	got := walkAll(t, s, "")

	assert.Equal(t, want, got, "names arrive sorted, which is what makes a namespace contiguous")
}

// TestWalkFragmentsAfterNeverSkipsAhead is the property the resume boundary
// rests on: pruning a subtree must never drop a name that sorts after the
// boundary. Getting this wrong silently skips objects — they would simply never
// be verified, with nothing to show for it.
func TestWalkFragmentsAfterNeverSkipsAhead(t *testing.T) {
	s := newStore(t, "d0")
	all := fragmentTree(t, s)

	// Every name is a boundary, plus the namespace directories themselves,
	// which is what the scrubber actually passes as a cursor.
	boundaries := append([]string(nil), all...)
	for _, name := range all {
		boundaries = append(boundaries, name[:len(name)-len(filepath.Base(name))-1])
	}

	boundaries = append(boundaries, "", "obj", "obj/zzz", "obj/0")

	for _, after := range boundaries {
		var want []string

		for _, name := range all {
			if name > after {
				want = append(want, name)
			}
		}

		assert.Equal(t, want, walkAll(t, s, after), "after=%q", after)
	}
}

// TestWalkFragmentsAfterDeletedCursor: a cursor may name a namespace that has
// since been deleted. The boundary is a position in an ordering, not a lookup,
// so everything past it must still be walked.
func TestWalkFragmentsAfterDeletedCursor(t *testing.T) {
	s := newStore(t, "d0")
	all := fragmentTree(t, s)

	// A namespace between the first and second object, which never existed.
	gone := "obj/" + "b7d3" + "00000000000000000000000000000000000000000000000000000000000" +
		"/1000" + "00000000000000000000000000000000000000000000000000000000000"

	var want []string

	for _, name := range all {
		if name > gone {
			want = append(want, name)
		}
	}

	require.NotEmpty(t, want)
	assert.Equal(t, want, walkAll(t, s, gone))
}

// TestWalkFragmentsSkipsBookkeeping: the occupancy checkpoint and the scrub
// cursor live at the disk root, and in-flight temp files live beside fragments.
// None of them is data.
func TestWalkFragmentsSkipsBookkeeping(t *testing.T) {
	root := filepath.Join(t.TempDir(), "d0")

	s, err := diskstore.New(map[cluster.DiskID]string{"d0": root})
	require.NoError(t, err)

	want := fragmentTree(t, s)

	require.NoError(t, s.ScrubStateStore().SaveScrubState("d0", cluster.ScrubState{Cursor: "obj/aa"}))
	require.NoError(t, s.Close())

	assert.Equal(t, want, walkAll(t, s, ""))
}

func TestWalkFragmentsStopsOnError(t *testing.T) {
	s := newStore(t, "d0")
	fragmentTree(t, s)

	boom := errors.New("stop here")

	var seen int

	err := s.WalkFragments(t.Context(), "d0", "", func(string) error {
		seen++

		return boom
	})

	require.ErrorIs(t, err, boom, "the callback's error surfaces unchanged")
	assert.Equal(t, 1, seen, "and the walk stops immediately")
}

func TestWalkFragmentsCanceled(t *testing.T) {
	s := newStore(t, "d0")
	fragmentTree(t, s)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := s.WalkFragments(ctx, "d0", "", func(string) error { return nil })
	require.ErrorIs(t, err, context.Canceled)
}

func TestWalkFragmentsUnknownDisk(t *testing.T) {
	s := newStore(t, "d0")

	require.Error(t, s.WalkFragments(t.Context(), "nope", "", func(string) error { return nil }))
}
