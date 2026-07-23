// Package bench is the go-faster/fs performance suite (ROADMAP Phase 10,
// DESIGN.md NFR-3). It measures the single-node storage backend — the "single
// NVMe node" the NFR-3 targets are stated against — and encodes those targets
// as regression gates:
//
//   - large-object throughput ≥ 80% of the same disk's raw sequential
//     bandwidth (a machine-independent ratio, robust on noisy CI);
//   - PUT allocations amortized O(1) per request — constant regardless of
//     object size (streaming, no full-object buffering);
//   - 4 KiB GET p99 latency under concurrent load.
//
// `go test ./bench -run NFR` runs the gates; `make bench` (or
// `go test ./bench -bench . -benchmem`) prints ns/op, MB/s and allocs/op for
// benchstat tracking.
package bench

import (
	"context"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/storagefs"
)

// object sizes exercised across the suite.
const (
	sizeSmall = 4 << 10   // 4 KiB — the small-object latency/allocs target.
	sizeMid   = 1 << 20   // 1 MiB.
	sizeLarge = 64 << 20  // 64 MiB — the large-object throughput target.
	sizeHuge  = 256 << 20 // 256 MiB — streaming-allocation headroom check.
)

// benchStore builds a filesystem backend under a temp dir with the fastest
// durability policy (SyncNone) — perf is measured against the OS page cache /
// device, not fsync latency, which the operator tunes separately.
func benchStore(tb testing.TB) (store *storagefs.Storage, dir string) {
	tb.Helper()

	dir = tb.TempDir()

	store, err := storagefs.New(dir, storagefs.WithSyncPolicy(storagefs.SyncNone))
	require.NoError(tb, err)
	require.NoError(tb, store.CreateBucket(context.Background(), "bench"))

	return store, dir
}

// deterministicBody yields n bytes without allocating per read: a fixed
// buffer streamed in a loop. This keeps the body source out of the
// measurement so PUT allocations reflect the backend alone.
type deterministicBody struct {
	remaining int64
	chunk     []byte
	pos       int
}

func newBody(n int64) *deterministicBody {
	chunk := make([]byte, 64<<10)
	for i := range chunk {
		chunk[i] = byte(i)
	}

	return &deterministicBody{remaining: n, chunk: chunk}
}

func (b *deterministicBody) Read(p []byte) (int, error) {
	if b.remaining <= 0 {
		return 0, io.EOF
	}

	n := copy(p, b.chunk[b.pos:])
	if int64(n) > b.remaining {
		n = int(b.remaining)
	}

	b.pos = (b.pos + n) % len(b.chunk)
	b.remaining -= int64(n)

	if b.remaining <= 0 {
		return n, io.EOF
	}

	return n, nil
}

func (b *deterministicBody) reset(n int64) {
	b.remaining = n
	b.pos = 0
}

// putObject writes one object of the given size, reusing body.
func putObject(tb testing.TB, s *storagefs.Storage, key string, size int64, body *deterministicBody) {
	tb.Helper()

	body.reset(size)

	_, err := s.PutObject(context.Background(), &fs.PutObjectRequest{
		Bucket: "bench",
		Key:    key,
		Size:   size,
		Reader: body,
	})
	require.NoError(tb, err)
}

// getObjectDiscard reads one object fully into io.Discard and returns bytes read.
func getObjectDiscard(tb testing.TB, s *storagefs.Storage, key string) int64 {
	tb.Helper()

	resp, err := s.GetObject(context.Background(), "bench", key)
	require.NoError(tb, err)

	n, err := io.Copy(io.Discard, resp.Reader)
	require.NoError(tb, err)
	require.NoError(tb, resp.Reader.Close())

	return n
}
