package fragment_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/fragment"
	"github.com/go-faster/fs/internal/cluster/scheme"
)

// fragStore is an in-memory fragment store driving the streaming callbacks the
// way clusterstore would with staging files.
type fragStore struct {
	data map[int][]byte
}

func newFragStore() *fragStore {
	return &fragStore{data: make(map[int][]byte)}
}

type storeSink struct {
	s   *fragStore
	idx int
	buf bytes.Buffer
}

func (w *storeSink) Write(p []byte) (int, error) { return w.buf.Write(p) }

func (w *storeSink) Close() error {
	w.s.data[w.idx] = append([]byte(nil), w.buf.Bytes()...)
	return nil
}

func (s *fragStore) sink(item fragment.Item) (io.WriteCloser, error) {
	return &storeSink{s: s, idx: item.Index}, nil
}

func (s *fragStore) reopen(item fragment.Item) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(s.data[item.Index])), nil
}

// openDropping returns an OpenFunc over the store that reports the given
// indices as lost.
func (s *fragStore) openDropping(lost ...int) fragment.OpenFunc {
	skip := make(map[int]struct{}, len(lost))
	for _, i := range lost {
		skip[i] = struct{}{}
	}

	return func(index int) (io.ReadCloser, error) {
		if _, ok := skip[index]; ok {
			return nil, nil
		}

		return io.NopCloser(bytes.NewReader(s.data[index])), nil
	}
}

// fileStage stages rebuilt shards in temp files, like clusterstore would.
func fileStage(t *testing.T) fragment.StageFunc {
	t.Helper()

	dir := t.TempDir()

	return func(index int) (io.ReadWriteSeeker, func(), error) {
		f, err := os.Create(filepath.Join(dir, "shard-"+strconv.Itoa(index))) //nolint:gosec // test temp path
		if err != nil {
			return nil, nil, err
		}

		return f, func() { _ = f.Close() }, nil
	}
}

func allSchemes() []scheme.Scheme {
	return []scheme.Scheme{
		{Kind: scheme.RF25},
		{Kind: scheme.RF3},
		{Kind: scheme.EC, K: 4, M: 2},
		{Kind: scheme.EC, K: 2, M: 1},
	}
}

// TestStreamMatchesInMemory: the streaming encoder must produce byte-identical
// fragments to the in-memory Encode — same placement, same RS math, same
// padding.
func TestStreamMatchesInMemory(t *testing.T) {
	top := topo(3, 3, 2)

	for _, s := range allSchemes() {
		for _, n := range []int{0, 1, 2, 999, 4096, 4097} {
			data := randBytes(n)

			mem, err := fragment.Encode(top, s, "k", data)
			require.NoError(t, err, "%s n=%d", s, n)

			plan, err := fragment.Plan(top, s, "k", int64(n))
			require.NoError(t, err, "%s n=%d", s, n)
			require.Len(t, plan, len(mem))

			store := newFragStore()
			err = fragment.EncodeStream(plan, s, int64(n), bytes.NewReader(data), store.sink, store.reopen)
			require.NoError(t, err, "%s n=%d", s, n)

			for i, f := range mem {
				assert.Equal(t, f.Target, plan[i].Target, "%s n=%d target %d", s, n, i)
				assert.Equal(t, f.Kind, plan[i].Kind, "%s n=%d kind %d", s, n, i)
				assert.Equal(t, int64(len(f.Data)), plan[i].Size, "%s n=%d planned size %d", s, n, i)
				assert.True(t, bytes.Equal(f.Data, store.data[i]),
					"%s n=%d fragment %d bytes must match in-memory encoder", s, n, i)
			}
		}
	}
}

func TestStreamRoundTrip(t *testing.T) {
	top := topo(3, 3, 2)

	for _, s := range allSchemes() {
		for _, n := range []int{0, 1, 999, 4097} {
			data := randBytes(n)

			plan, err := fragment.Plan(top, s, "k", int64(n))
			require.NoError(t, err)

			store := newFragStore()
			require.NoError(t,
				fragment.EncodeStream(plan, s, int64(n), bytes.NewReader(data), store.sink, store.reopen))

			var out bytes.Buffer

			err = fragment.DecodeStream(s, int64(n), store.openDropping(), nil, &out)
			require.NoError(t, err, "%s n=%d", s, n)
			assert.True(t, bytes.Equal(data, out.Bytes()), "%s n=%d round-trip", s, n)
		}
	}
}

func TestStreamTwoPhaseParity(t *testing.T) {
	// Data phase first (what a quorum ack waits for), parity phase separately —
	// the async path. The result must be byte-identical to the one-shot encoder.
	top := topo(3, 3, 2)
	data := randBytes(3000)

	for _, s := range allSchemes() {
		plan, err := fragment.Plan(top, s, "k", int64(len(data)))
		require.NoError(t, err)

		oneShot := newFragStore()
		require.NoError(t,
			fragment.EncodeStream(plan, s, int64(len(data)), bytes.NewReader(data), oneShot.sink, oneShot.reopen))

		twoPhase := newFragStore()
		require.NoError(t,
			fragment.EncodeDataStream(plan, s, int64(len(data)), bytes.NewReader(data), twoPhase.sink),
			"%s data phase", s)
		require.NoError(t,
			fragment.EncodeParityStream(plan, s, int64(len(data)), twoPhase.sink, twoPhase.reopen),
			"%s parity phase", s)

		require.Equal(t, len(oneShot.data), len(twoPhase.data), "%s fragment count", s)

		for i := range plan {
			assert.True(t, bytes.Equal(oneShot.data[i], twoPhase.data[i]),
				"%s fragment %d: two-phase must match one-shot", s, i)
		}
	}
}

func TestStreamDegradedECRead(t *testing.T) {
	s := scheme.Scheme{Kind: scheme.EC, K: 4, M: 2}
	top := topo(3, 3, 2)
	data := randBytes(5000)

	plan, err := fragment.Plan(top, s, "k", int64(len(data)))
	require.NoError(t, err)

	store := newFragStore()
	require.NoError(t,
		fragment.EncodeStream(plan, s, int64(len(data)), bytes.NewReader(data), store.sink, store.reopen))

	// Any m=2 lost shards reconstruct (staged rebuild).
	for _, lost := range combinations(s.K+s.M, s.M) {
		var out bytes.Buffer

		err := fragment.DecodeStream(s, int64(len(data)), store.openDropping(lost...), fileStage(t), &out)
		require.NoError(t, err, "lost=%v", lost)
		assert.True(t, bytes.Equal(data, out.Bytes()), "lost=%v", lost)
	}

	// m+1 lost is unrecoverable.
	for _, lost := range combinations(s.K+s.M, s.M+1) {
		var out bytes.Buffer

		err := fragment.DecodeStream(s, int64(len(data)), store.openDropping(lost...), fileStage(t), &out)
		require.ErrorIs(t, err, fragment.ErrUnrecoverable, "lost=%v", lost)
	}
}

func TestStreamDegradedNeedsStage(t *testing.T) {
	s := scheme.Scheme{Kind: scheme.EC, K: 2, M: 1}
	top := topo(3, 2, 2)
	data := randBytes(100)

	plan, err := fragment.Plan(top, s, "k", int64(len(data)))
	require.NoError(t, err)

	store := newFragStore()
	require.NoError(t,
		fragment.EncodeStream(plan, s, int64(len(data)), bytes.NewReader(data), store.sink, store.reopen))

	var out bytes.Buffer

	// Degraded read with no stage must fail loudly, not buffer silently.
	err = fragment.DecodeStream(s, int64(len(data)), store.openDropping(0), nil, &out)
	require.Error(t, err)
	assert.NotErrorIs(t, err, fragment.ErrUnrecoverable, "recoverable, just missing staging")
}

func TestStreamReplicaTolerance(t *testing.T) {
	top := topo(3, 2, 2)
	data := randBytes(700)

	for _, s := range []scheme.Scheme{{Kind: scheme.RF25}, {Kind: scheme.RF3}} {
		plan, err := fragment.Plan(top, s, "k", int64(len(data)))
		require.NoError(t, err)

		store := newFragStore()
		require.NoError(t,
			fragment.EncodeStream(plan, s, int64(len(data)), bytes.NewReader(data), store.sink, store.reopen))

		for lost := range s.FullReplicas() {
			var out bytes.Buffer

			err := fragment.DecodeStream(s, int64(len(data)), store.openDropping(lost), nil, &out)
			require.NoError(t, err, "%s lost=%d", s, lost)
			assert.True(t, bytes.Equal(data, out.Bytes()))
		}

		// All full replicas lost → unrecoverable on the read path.
		allFull := make([]int, s.FullReplicas())
		for i := range allFull {
			allFull[i] = i
		}

		var out bytes.Buffer

		err = fragment.DecodeStream(s, int64(len(data)), store.openDropping(allFull...), nil, &out)
		require.ErrorIs(t, err, fragment.ErrUnrecoverable, "%s", s)
	}
}

// TestStreamRepairHalf checks the streaming RF=2.5 half-repair against the
// in-memory one.
func TestStreamRepairHalf(t *testing.T) {
	for _, n := range []int{1, 2, 9, 4097} {
		data := randBytes(n)

		parity, err := scheme.HalfParity(data)
		require.NoError(t, err)

		c, err := scheme.NewCodec(2, 1)
		require.NoError(t, err)

		shards, err := c.Encode(data)
		require.NoError(t, err)

		// Lose the first half; rebuild it streaming from the second + parity.
		var rebuilt bytes.Buffer

		err = scheme.RepairHalfStream(bytes.NewReader(shards[1]), 1, bytes.NewReader(parity), int64(n), &rebuilt)
		require.NoError(t, err, "n=%d", n)
		assert.True(t, bytes.Equal(shards[0], rebuilt.Bytes()), "n=%d rebuilt first half", n)

		// Symmetric: lose the second half.
		rebuilt.Reset()

		err = scheme.RepairHalfStream(bytes.NewReader(shards[0]), 0, bytes.NewReader(parity), int64(n), &rebuilt)
		require.NoError(t, err, "n=%d", n)
		assert.True(t, bytes.Equal(shards[1], rebuilt.Bytes()), "n=%d rebuilt second half", n)
	}
}
