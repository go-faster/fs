package bench

import (
	"crypto/md5" //nolint:gosec // MD5 mirrors the S3 ETag the backend must compute.
	"hash"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// rawSequentialWrite measures the disk's raw sequential write bandwidth
// (bytes/sec) under the given directory: create → stream size bytes → close,
// no fsync, matching the SyncNone backend policy. When hashETag is set the
// stream is also fed through MD5 — the ceiling a compliant S3 PUT can hit,
// since the ETag mandates hashing every byte and MD5 is slower than NVMe
// sequential write. It writes total/size files and returns the aggregate rate.
func rawSequentialWrite(tb testing.TB, dir string, size, total int64, hashETag bool) float64 {
	tb.Helper()

	chunk := make([]byte, 64<<10)
	for i := range chunk {
		chunk[i] = byte(i)
	}

	var h hash.Hash
	if hashETag {
		h = md5.New() //nolint:gosec // ETag parity, not security.
	}

	files := max(total/size, 1)
	start := time.Now()

	var written int64

	for f := range files {
		path := filepath.Join(dir, "raw-w-"+itoa(f))

		file, err := os.Create(path) //nolint:gosec // Benchmark scratch file under t.TempDir().
		require.NoError(tb, err)

		var w io.Writer = file
		if h != nil {
			w = io.MultiWriter(file, h)
		}

		remaining := size
		for remaining > 0 {
			n := min(int64(len(chunk)), remaining)

			m, err := w.Write(chunk[:n])
			require.NoError(tb, err)

			remaining -= int64(m)
			written += int64(m)
		}

		require.NoError(tb, file.Close())
	}

	return float64(written) / time.Since(start).Seconds()
}

// rawSequentialRead measures raw sequential read bandwidth from a file the
// backend would also serve out of the page cache — the honest baseline for
// comparing GET, which reads the same warm cache.
func rawSequentialRead(tb testing.TB, dir string, size int64) float64 {
	tb.Helper()

	path := filepath.Join(dir, "raw-r")
	require.NoError(tb, os.WriteFile(path, make([]byte, size), 0o600))

	// Warm the cache once so the measurement matches a freshly-written GET.
	f, err := os.Open(path) //nolint:gosec // Benchmark scratch file.
	require.NoError(tb, err)

	_, _ = io.Copy(io.Discard, f)
	_ = f.Close()

	start := time.Now()

	f, err = os.Open(path) //nolint:gosec // Benchmark scratch file.
	require.NoError(tb, err)

	n, err := io.Copy(io.Discard, f)
	require.NoError(tb, err)
	require.NoError(tb, f.Close())

	return float64(n) / time.Since(start).Seconds()
}

// itoa is a tiny allocation-free-ish int formatter for scratch file names.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}

	var buf [20]byte

	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}

	return string(buf[i:])
}
