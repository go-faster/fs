package clusterstore

import (
	"context"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-faster/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster"
)

// memScrubState is an in-memory ScrubStateStore, standing in for the per-disk
// files a node writes.
type memScrubState struct {
	mu     sync.Mutex
	states map[cluster.DiskID]cluster.ScrubState
	err    error
}

func newMemScrubState() *memScrubState {
	return &memScrubState{states: make(map[cluster.DiskID]cluster.ScrubState)}
}

func (m *memScrubState) LoadScrubState(disk cluster.DiskID) cluster.ScrubState {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.states[disk]
}

func (m *memScrubState) SaveScrubState(disk cluster.DiskID, state cluster.ScrubState) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.err != nil {
		return m.err
	}

	m.states[disk] = state

	return nil
}

func (m *memScrubState) get(disk cluster.DiskID) cluster.ScrubState {
	m.mu.Lock()
	defer m.mu.Unlock()

	return m.states[disk]
}

func (m *memScrubState) set(disk cluster.DiskID, state cluster.ScrubState) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.states[disk] = state
}

// scrubbedObjects records which objects a pass fed through repair.
func scrubbedObjects(t *testing.T, r *Repairer, ctx context.Context) *ScrubReport {
	t.Helper()

	rep, err := r.Scrub(ctx)
	require.NoError(t, err)

	return rep
}

// TestScrubRecordsCompletion covers the coverage signal: a pass that runs to
// the end leaves no cursor and stamps when the disk was last fully verified.
func TestScrubRecordsCompletion(t *testing.T) {
	fc := newFakeCluster(3, 1)
	c := fc.coordinator(t, Config{})

	for _, key := range []string{"a", "b", "c"} {
		mustPut(t, c, key, randBytes(500))
	}

	c.Flush()

	state := newMemScrubState()

	r, err := NewRepairer(RepairerConfig{
		Coordinator: c,
		Self:        fc.topo.Nodes[0].ID,
		ScrubState:  state,
	})
	require.NoError(t, err)

	before := time.Now().UTC()

	scrubbedObjects(t, r, t.Context())

	got := state.get("d0")
	assert.Empty(t, got.Cursor, "a completed pass leaves nothing to resume from")
	assert.False(t, got.LastCompleted.IsZero(), "completion is stamped")
	assert.False(t, got.LastCompleted.Before(before))
}

// TestScrubResumesFromCursor is the point of the whole change: a pass
// interrupted partway does not re-verify what it already did.
func TestScrubResumesFromCursor(t *testing.T) {
	fc := newFakeCluster(3, 1)
	c := fc.coordinator(t, Config{})

	for _, key := range []string{"a", "b", "c", "d", "e"} {
		mustPut(t, c, key, randBytes(500))
	}

	c.Flush()

	state := newMemScrubState()
	self := fc.topo.Nodes[0].ID

	newR := func() *Repairer {
		r, err := NewRepairer(RepairerConfig{Coordinator: c, Self: self, ScrubState: state})
		require.NoError(t, err)

		return r
	}

	// A full pass, to learn how much this node holds.
	full := scrubbedObjects(t, newR(), t.Context())
	require.Positive(t, full.Objects, "the node must hold something to scrub")

	// Now pretend that pass died just after its first namespace: the cursor
	// names it, and the disk is mid-pass.
	names, err := fc.stores[self].List(context.Background(), "d0", "obj/")
	require.NoError(t, err)
	require.NotEmpty(t, names)

	first := names[0][:strings.LastIndex(names[0], "/")]

	state.set("d0", cluster.ScrubState{Cursor: first, PassStarted: time.Now().UTC()})

	resumed := scrubbedObjects(t, newR(), t.Context())

	assert.Less(t, resumed.Objects, full.Objects,
		"a resumed pass skips what the interrupted one already verified")

	// And it finishes the disk: the cursor is cleared and completion stamped.
	got := state.get("d0")
	assert.Empty(t, got.Cursor)
	assert.False(t, got.LastCompleted.IsZero())
}

// TestScrubCanceledNeverLooksComplete: an interrupted pass must not stamp
// completion. LastCompleted is read as "every object on this disk was verified
// by then", so a pass that stopped early claiming it would hide exactly the
// coverage gap the timestamp exists to expose.
func TestScrubCanceledNeverLooksComplete(t *testing.T) {
	fc := newFakeCluster(3, 1)
	c := fc.coordinator(t, Config{})

	for _, key := range []string{"a", "b", "c"} {
		mustPut(t, c, key, randBytes(300))
	}

	c.Flush()

	state := newMemScrubState()

	r, err := NewRepairer(RepairerConfig{
		Coordinator: c,
		Self:        fc.topo.Nodes[0].ID,
		ScrubState:  state,
	})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = r.Scrub(ctx)
	require.Error(t, err, "a canceled scrub reports that it stopped early")

	assert.True(t, state.get("d0").LastCompleted.IsZero(),
		"a pass that did not finish must not claim the disk was covered")
}

// TestScrubWithoutStateStore checks the feature is optional: a repairer with no
// store still scrubs, it just always starts from the beginning.
func TestScrubWithoutStateStore(t *testing.T) {
	fc := newFakeCluster(3, 1)
	c := fc.coordinator(t, Config{})

	mustPut(t, c, "a", randBytes(500))
	c.Flush()

	r := newRepairer(t, c, fc.topo.Nodes[0].ID, false)

	first := scrubbedObjects(t, r, t.Context())
	second := scrubbedObjects(t, r, t.Context())

	assert.Equal(t, first.Objects, second.Objects, "every pass is a full pass")
}

// TestScrubSaveFailureDoesNotStopTheSweep: a cursor that will not persist costs
// a repeated pass. Refusing to verify data over it would cost the data.
func TestScrubSaveFailureDoesNotStopTheSweep(t *testing.T) {
	fc := newFakeCluster(3, 1)
	c := fc.coordinator(t, Config{})

	mustPut(t, c, "a", randBytes(500))
	c.Flush()

	state := newMemScrubState()
	state.err = errors.New("disk full")

	var reported int

	r, err := NewRepairer(RepairerConfig{
		Coordinator: c,
		Self:        fc.topo.Nodes[0].ID,
		ScrubState:  state,
		OnError:     func(string, string, error) { reported++ },
	})
	require.NoError(t, err)

	rep, err := r.Scrub(t.Context())
	require.NoError(t, err, "the sweep completes despite the cursor not persisting")
	assert.Positive(t, rep.Objects)
	assert.Positive(t, reported, "and the failure is surfaced, not swallowed")
}

// TestScrubOrderPrefersInterruptedThenStalest checks the scheduler that keeps a
// restart-prone node from starving its last disks: whatever was mid-pass goes
// first, then whatever has gone longest without a complete sweep.
func TestScrubOrderPrefersInterruptedThenStalest(t *testing.T) {
	fc := newFakeCluster(3, 3)
	c := fc.coordinator(t, Config{})

	state := newMemScrubState()

	now := time.Now().UTC()
	state.set("d0", cluster.ScrubState{LastCompleted: now})
	state.set("d1", cluster.ScrubState{LastCompleted: now.Add(-72 * time.Hour)})
	state.set("d2", cluster.ScrubState{Cursor: "obj/zz", PassStarted: now.Add(-time.Hour)})

	r, err := NewRepairer(RepairerConfig{
		Coordinator: c,
		Self:        fc.topo.Nodes[0].ID,
		ScrubState:  state,
	})
	require.NoError(t, err)

	order := r.scrubOrder([]cluster.Disk{{ID: "d0"}, {ID: "d1"}, {ID: "d2"}})

	assert.Equal(t, []cluster.DiskID{"d2", "d1", "d0"}, order,
		"interrupted first, then least recently completed")
}

// listWalker streams a store's names through the FragmentWalker interface by
// reusing its buffered listing. It exercises the scrubber's streaming consumer
// — the accumulate-one-namespace-at-a-time loop — independently of the disk
// walker that feeds it in production.
type listWalker struct {
	store interface {
		List(ctx context.Context, disk cluster.DiskID, prefix string) ([]string, error)
	}
	walked int
}

func (w *listWalker) WalkFragments(ctx context.Context, disk cluster.DiskID, after string, fn func(string) error) error {
	names, err := w.store.List(ctx, disk, "obj/")
	if err != nil {
		return err
	}

	sort.Strings(names)

	for _, name := range names {
		if name <= after {
			continue
		}

		w.walked++

		if err := fn(name); err != nil {
			return err
		}
	}

	return nil
}

// TestScrubStreamingMatchesBuffered pins the rewrite: sweeping a disk by
// streaming names must find and repair exactly what materializing them did.
func TestScrubStreamingMatchesBuffered(t *testing.T) {
	run := func(t *testing.T, withWalker bool) *ScrubReport {
		t.Helper()

		fc := newFakeCluster(3, 2)
		c := fc.coordinator(t, Config{})

		for _, key := range []string{"a", "b/c", "d", "e/f/g", "h"} {
			mustPut(t, c, key, randBytes(400))
		}

		c.Flush()

		self := fc.topo.Nodes[0].ID

		cfg := RepairerConfig{Coordinator: c, Self: self, Verify: true}
		if withWalker {
			cfg.Fragments = &listWalker{store: fc.stores[self]}
		}

		r, err := NewRepairer(cfg)
		require.NoError(t, err)

		rep, err := r.Scrub(t.Context())
		require.NoError(t, err)

		return rep
	}

	buffered := run(t, false)
	streamed := run(t, true)

	assert.Equal(t, buffered.Objects, streamed.Objects, "the same objects are swept")
	assert.Equal(t, buffered.UnknownDirs, streamed.UnknownDirs)
	assert.Equal(t, buffered.Failed, streamed.Failed)
	assert.Positive(t, buffered.Objects, "and there was something to sweep")
}

// TestScrubStreamingResumeSkipsWalkedNames checks the two halves fit together:
// a resumed pass tells the walker where it stopped, so the names it already
// verified are never streamed again — the point of pruning on a large disk.
func TestScrubStreamingResumeSkipsWalkedNames(t *testing.T) {
	fc := newFakeCluster(3, 1)
	c := fc.coordinator(t, Config{})

	for _, key := range []string{"a", "b", "c", "d", "e"} {
		mustPut(t, c, key, randBytes(300))
	}

	c.Flush()

	self := fc.topo.Nodes[0].ID
	state := newMemScrubState()

	newR := func() (*Repairer, *listWalker) {
		w := &listWalker{store: fc.stores[self]}

		r, err := NewRepairer(RepairerConfig{
			Coordinator: c,
			Self:        self,
			ScrubState:  state,
			Fragments:   w,
		})
		require.NoError(t, err)

		return r, w
	}

	first, fullWalk := newR()
	_, err := first.Scrub(t.Context())
	require.NoError(t, err)
	require.Positive(t, fullWalk.walked)

	// Interrupt partway and sweep again. The cursor is a namespace directory,
	// and that namespace's own entries sort after it, so only the namespaces
	// strictly before it are pruned — pick one with some behind it.
	names, err := fc.stores[self].List(context.Background(), "d0", "obj/")
	require.NoError(t, err)
	require.NotEmpty(t, names)

	sort.Strings(names)

	var dirs []string

	for _, name := range names {
		dir := name[:strings.LastIndex(name, "/")]
		if len(dirs) == 0 || dirs[len(dirs)-1] != dir {
			dirs = append(dirs, dir)
		}
	}

	require.Greater(t, len(dirs), 1, "the disk must hold more than one namespace")

	state.set("d0", cluster.ScrubState{
		Cursor:      dirs[len(dirs)/2],
		PassStarted: time.Now().UTC(),
	})

	second, resumedWalk := newR()
	_, err = second.Scrub(t.Context())
	require.NoError(t, err)

	assert.Less(t, resumedWalk.walked, fullWalk.walked,
		"a resumed sweep does not re-stream what it already verified")
}
