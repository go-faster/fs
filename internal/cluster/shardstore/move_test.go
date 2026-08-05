package shardstore_test

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/rangemap"
	"github.com/go-faster/fs/internal/cluster/shardstore"
	"github.com/go-faster/fs/internal/cluster/transport"
)

// openShardAt opens a shard at a caller-chosen directory, so a test can close
// one and open another over the same files — which is the only way to tell a
// cursor that was persisted from one that was merely remembered.
func openShardAt(t *testing.T, dir string, follows ...rangemap.Range) *shardstore.Shard {
	t.Helper()

	s, err := shardstore.OpenShard(dir)
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })

	s.Follow(follows)

	return s
}

// stepSource is an owner's shard with a fault injected: it records the cursors
// it was asked for, and fails once it has answered a set number of steps.
//
// The recording is what the resume tests assert on. Where a backfill picks up is
// not visible in the data — a resumed run and a restarted one both end with the
// same range copied — so the only way to catch a cursor that was ignored is to
// watch what the next run asks for.
type stepSource struct {
	from *shardstore.Shard
	// failAfter is how many steps to answer before failing. Zero never fails.
	failAfter int

	asked []string
	steps int
}

func (s *stepSource) ReadBackfill(
	ctx context.Context,
	r rangemap.Range,
	cursor string,
	limit int,
) (shardstore.BackfillStep, error) {
	s.asked = append(s.asked, cursor)

	if s.failAfter > 0 && s.steps >= s.failAfter {
		return shardstore.BackfillStep{}, assert.AnError
	}

	s.steps++

	return s.from.ReadBackfill(ctx, r, cursor, limit)
}

// TestBackfillCopiesTheWholeRange is the driver end to end: the learner pulls
// step after step until the owner says the range is exhausted.
func TestBackfillCopiesTheWholeRange(t *testing.T) {
	owner := openShard(t, learned)
	learner := openShardAt(t, t.TempDir(), learned)

	fill(t, owner, "photos", 20)

	got, err := learner.Backfill(t.Context(), learned, owner, 6)
	require.NoError(t, err)

	assert.True(t, got.Done)
	assert.Equal(t, 20, got.Entries)
	assert.Greater(t, got.Steps, 1, "twenty entries at six a step is more than one step")

	learner.Adopt([]rangemap.Range{learned})
	assert.Equal(t, scanKeys(t, owner, "photos"), scanKeys(t, learner, "photos"))

	want, err := owner.Usage(t.Context(), "photos")
	require.NoError(t, err)

	usage, err := learner.Usage(t.Context(), "photos")
	require.NoError(t, err)
	assert.Equal(t, want, usage, "the counters moved with the entries")
}

// TestBackfillResumesAfterACrash is the property the cursor exists for. A
// killed backfill must not start over: on a range worth moving, starting over
// is the cost that would make moves impractical.
func TestBackfillResumesAfterACrash(t *testing.T) {
	owner := openShard(t, learned)
	fill(t, owner, "photos", 20)

	dir := t.TempDir()

	learner := openShardAt(t, dir, learned)

	// Two steps land, the third fails.
	faulty := &stepSource{from: owner, failAfter: 2}

	got, err := learner.Backfill(t.Context(), learned, faulty, 5)
	require.Error(t, err)
	assert.Equal(t, 2, got.Steps)
	assert.Equal(t, 10, got.Entries)

	require.NoError(t, learner.Close())

	// A new process, over the same files.
	resumed := openShardAt(t, dir, learned)
	healthy := &stepSource{from: owner}

	got, err = resumed.Backfill(t.Context(), learned, healthy, 5)
	require.NoError(t, err)
	assert.True(t, got.Done)

	require.NotEmpty(t, healthy.asked)
	assert.NotEmpty(t, healthy.asked[0],
		"the second run started from the beginning: the cursor did not survive the restart")
	assert.Equal(t, 10, got.Entries, "the resumed run re-copied what the first one already had")

	resumed.Adopt([]rangemap.Range{learned})
	assert.Len(t, scanKeys(t, resumed, "photos"), 20)
}

// poisonedSource answers with real steps whose entries will be refused: the
// cursor advances, and nothing it describes can land.
//
// It is the failure between the two writes — the one a source that simply
// errors cannot produce, because that fault arrives before either of them.
type poisonedSource struct {
	from *shardstore.Shard
	// honest is how many steps are answered intact before the poisoned one.
	honest int
	steps  int
}

func (p *poisonedSource) ReadBackfill(
	ctx context.Context,
	r rangemap.Range,
	cursor string,
	limit int,
) (shardstore.BackfillStep, error) {
	step, err := p.from.ReadBackfill(ctx, r, cursor, limit)
	if err != nil {
		return step, err
	}

	p.steps++

	if p.steps <= p.honest {
		return step, nil
	}

	// The cursor is the owner's real one — the walk did reach there — but the
	// entries are outside the range, so the learner refuses the whole step.
	step.Entries = []metastore.Entry{entry("zulu", "poison.jpg", 1, 1)}

	return step, nil
}

// TestBackfillRecordsTheCursorAfterTheEntries is the ordering that makes a crash
// survivable, and the one that would be silent if it were wrong.
//
// A cursor is a claim that everything below it is already here. Written before
// the entries it describes, a failure in between turns that claim into a hole in
// the middle of the range — objects that exist and read as absent, on a learner
// that looks complete and would be promoted.
//
// So the step that fails must leave the cursor where the last step that landed
// put it, and the resume that follows must copy what the failed one did not.
func TestBackfillRecordsTheCursorAfterTheEntries(t *testing.T) {
	// Bounded, so there is a key outside it for the poisoned step to carry.
	bounded := rangemap.Range{Start: "", End: "om", Owner: "n0", Learners: []cluster.NodeID{"n1"}}

	owner := openShard(t, bounded)
	fill(t, owner, "alpha", 20)

	learner := openShardAt(t, t.TempDir(), bounded)

	_, err := learner.Backfill(t.Context(), bounded, &poisonedSource{from: owner, honest: 2}, 4)
	require.ErrorContains(t, err, "outside the range")

	// The resume picks up from the cursor the failure left behind.
	_, err = learner.Backfill(t.Context(), bounded, owner, 4)
	require.NoError(t, err)

	learner.Adopt([]rangemap.Range{bounded})
	assert.Len(t, scanKeys(t, learner, "alpha"), 20,
		"the cursor moved past a step whose entries never landed")
}

// TestBackfillForgetsItsCursorWhenDone: a completed cursor left behind is the
// one way this design could serve a hole.
//
// A learner promoted, later moved away and later still made a learner of the
// same range again would resume from a position describing data it no longer
// has — and skip straight to serving a range it never copied.
func TestBackfillForgetsItsCursorWhenDone(t *testing.T) {
	owner := openShard(t, learned)
	fill(t, owner, "photos", 8)

	learner := openShardAt(t, t.TempDir(), learned)

	_, err := learner.Backfill(t.Context(), learned, owner, 3)
	require.NoError(t, err)

	again := &stepSource{from: owner}

	_, err = learner.Backfill(t.Context(), learned, again, 3)
	require.NoError(t, err)

	require.NotEmpty(t, again.asked)
	assert.Empty(t, again.asked[0],
		"a finished backfill left a cursor behind, which a later one would resume from")
}

// TestBackfillStartsOverWhenTheRangeChanged: a cursor is only meaningful against
// the range it was taken in. A split leaves a range that starts in the same
// place and ends somewhere else, and that is a different range.
func TestBackfillStartsOverWhenTheRangeChanged(t *testing.T) {
	half := rangemap.Range{Start: "", End: "om", Owner: "n0", Learners: []cluster.NodeID{"n1"}}

	owner := openShard(t, learned)
	fill(t, owner, "alpha", 12)

	learner := openShardAt(t, t.TempDir(), learned)

	faulty := &stepSource{from: owner, failAfter: 1}
	_, err := learner.Backfill(t.Context(), learned, faulty, 4)
	require.Error(t, err)

	// The range is split under the move, so what is being copied now is [_,om).
	owner.Adopt([]rangemap.Range{half})
	learner.Follow([]rangemap.Range{half})

	healthy := &stepSource{from: owner}

	_, err = learner.Backfill(t.Context(), half, healthy, 4)
	require.NoError(t, err)

	require.NotEmpty(t, healthy.asked)
	assert.Empty(t, healthy.asked[0],
		"a cursor from a different range was resumed from")
}

// TestBackfillStartsOverWhenTheOwnerChanged: a cursor claims that what is below
// it matches the owner's contents. After a failover the promoted node's contents
// may differ below that point — it holds the records it received and not the
// ones it did not, and nothing tells them apart.
func TestBackfillStartsOverWhenTheOwnerChanged(t *testing.T) {
	promoted := learned
	promoted.Owner = "n2"

	owner := openShard(t, learned)
	fill(t, owner, "photos", 12)

	learner := openShardAt(t, t.TempDir(), learned)

	faulty := &stepSource{from: owner, failAfter: 1}
	_, err := learner.Backfill(t.Context(), learned, faulty, 4)
	require.Error(t, err)

	owner.Adopt([]rangemap.Range{promoted})
	learner.Follow([]rangemap.Range{promoted})

	healthy := &stepSource{from: owner}

	_, err = learner.Backfill(t.Context(), promoted, healthy, 4)
	require.NoError(t, err)

	require.NotEmpty(t, healthy.asked)
	assert.Empty(t, healthy.asked[0],
		"a cursor taken from a node that no longer owns the range was resumed from")
}

// TestBackfillRefusesARangeItDoesNotLearn: a shard pulling a range the map never
// told it to learn would fill itself with data nothing routes to it.
//
// Refused before the owner is asked, not after. Learn would decline every entry
// anyway, but only once a step had crossed the network — so a node working from
// a stale map would read a peer's range in full to be told it should not have.
func TestBackfillRefusesARangeItDoesNotLearn(t *testing.T) {
	owner := openShard(t, learned)
	fill(t, owner, "photos", 8)

	learner := openShardAt(t, t.TempDir())

	from := &stepSource{from: owner}

	_, err := learner.Backfill(t.Context(), learned, from, 4)
	require.ErrorIs(t, err, shardstore.ErrNotFollowed)

	assert.Empty(t, from.asked, "the owner was read for a range this shard does not learn")
}

// stubborn answers every step with the same cursor and never finishes.
type stubborn struct{}

func (stubborn) ReadBackfill(
	context.Context, rangemap.Range, string, int,
) (shardstore.BackfillStep, error) {
	return shardstore.BackfillStep{Cursor: "oa", Done: false}, nil
}

// TestBackfillRefusesACursorThatDoesNotAdvance: the loop ends because the cursor
// moves. A peer that answered otherwise would have this node spinning against it
// forever, and nothing about that would look like an error.
func TestBackfillRefusesACursorThatDoesNotAdvance(t *testing.T) {
	learner := openShardAt(t, t.TempDir(), learned)

	_, err := learner.Backfill(t.Context(), learned, stubborn{}, 4)
	require.ErrorContains(t, err, "without finishing the range")
}

// TestResetForgetsTheBackfillCursor: a reset removes the entries, so a cursor
// that survived it would claim a prefix that is no longer there.
func TestResetForgetsTheBackfillCursor(t *testing.T) {
	owner := openShard(t, learned)
	fill(t, owner, "photos", 12)

	learner := openShardAt(t, t.TempDir(), learned)

	faulty := &stepSource{from: owner, failAfter: 1}
	_, err := learner.Backfill(t.Context(), learned, faulty, 4)
	require.Error(t, err)

	require.NoError(t, learner.Reset(t.Context()))

	healthy := &stepSource{from: owner}

	_, err = learner.Backfill(t.Context(), learned, healthy, 4)
	require.NoError(t, err)

	require.NotEmpty(t, healthy.asked)
	assert.Empty(t, healthy.asked[0],
		"the cursor outlived the entries it described")

	learner.Adopt([]rangemap.Range{learned})
	assert.Len(t, scanKeys(t, learner, "photos"), 12)
}

// TestBackfillOverTheWire: the move's one remote operation, against a real peer.
//
// The learner pulls, so this is the whole transport surface a move needs — the
// write half never leaves the node, because the learner stores what it pulled
// into its own shard.
func TestBackfillOverTheWire(t *testing.T) {
	owner := openShard(t, learned)
	fill(t, owner, "photos", 25)

	srv := httptest.NewServer(transport.NewServer(
		transport.NewMemStore(), peerSecret, transport.WithShard(shardstore.Serve(owner)),
	))
	t.Cleanup(srv.Close)

	client, err := transport.NewClient(srv.URL, peerSecret, "n0", srv.Client())
	require.NoError(t, err)

	learner := openShardAt(t, t.TempDir(), learned)

	got, err := learner.Backfill(t.Context(), learned, shardstore.NewPeer(client), 7)
	require.NoError(t, err)

	assert.True(t, got.Done)
	assert.Equal(t, 25, got.Entries)
	assert.Greater(t, got.Steps, 3)

	learner.Adopt([]rangemap.Range{learned})
	assert.Equal(t, scanKeys(t, owner, "photos"), scanKeys(t, learner, "photos"))
}

// TestBackfillOverTheWireRefusesAnUnservedRange: a peer asked for a range it
// does not own must refuse rather than answer with what it happens to hold —
// the refusal is what stops a stale map from producing a half-copied learner.
func TestBackfillOverTheWireRefusesAnUnservedRange(t *testing.T) {
	owner := openShard(t, rangemap.Range{Start: "", End: "om", Owner: "n0"})

	srv := httptest.NewServer(transport.NewServer(
		transport.NewMemStore(), peerSecret, transport.WithShard(shardstore.Serve(owner)),
	))
	t.Cleanup(srv.Close)

	client, err := transport.NewClient(srv.URL, peerSecret, "n0", srv.Client())
	require.NoError(t, err)

	_, err = shardstore.NewPeer(client).ReadBackfill(t.Context(), learned, "", 4)
	require.ErrorIs(t, err, shardstore.ErrNotOwned, "the sentinel survives the wire")
}
