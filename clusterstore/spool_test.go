package clusterstore

import (
	"bytes"
	"crypto/rand"
	"io"
	"os"
	"testing"

	"github.com/go-faster/errors"
	"github.com/stretchr/testify/require"
)

// unsized hides the length of a body the way an http.Request with
// Transfer-Encoding: chunked does: readable, but with nothing to measure short
// of draining it.
type unsized struct{ r io.Reader }

func (u unsized) Read(p []byte) (int, error) { return u.r.Read(p) } //nolint:wrapcheck // Test shim.

func TestSpool(t *testing.T) {
	for _, tt := range []struct {
		name string
		size int
	}{
		{"empty", 0},
		{"tiny", 3},
		{"just under the threshold", spoolThreshold - 1},
		{"exactly the threshold", spoolThreshold},
		{"over the threshold, spills to disk", spoolThreshold + 1},
		{"well over the threshold", spoolThreshold*2 + 12345},
	} {
		t.Run(tt.name, func(t *testing.T) {
			want := make([]byte, tt.size)
			_, err := rand.Read(want)
			require.NoError(t, err)

			body, size, cleanup, err := spool(unsized{r: bytes.NewReader(want)})
			require.NoError(t, err)

			defer cleanup()

			require.Equal(t, int64(tt.size), size)

			got, err := io.ReadAll(body)
			require.NoError(t, err)
			require.Equal(t, want, got, "body round-trips byte for byte")
		})
	}
}

// TestSpoolLeavesNothingBehind checks that a spilled body's temp file is gone
// once cleanup has run. How it gets there is platform-specific — POSIX unlinks
// at creation, Windows cannot remove an open file and defers it to cleanup —
// so the invariant is asserted where both must agree.
func TestSpoolLeavesNothingBehind(t *testing.T) {
	body, _, cleanup, err := spool(unsized{r: bytes.NewReader(make([]byte, spoolThreshold+1))})
	require.NoError(t, err)

	f, ok := body.(*os.File)
	require.True(t, ok, "a body past the threshold spills to a file")

	name := f.Name()
	require.NotEmpty(t, name)

	cleanup()

	_, err = os.Stat(name)
	require.ErrorIs(t, err, os.ErrNotExist, "the spool file must not outlive cleanup")
}

// TestSpoolReadError checks that a body that fails mid-stream surfaces the
// error rather than committing a truncated object.
func TestSpoolReadError(t *testing.T) {
	boom := errors.New("connection reset")

	for _, tt := range []struct {
		name string
		// bytes delivered before the failure; past the threshold the failure
		// happens while spilling to the temp file rather than while filling
		// the in-memory buffer.
		before int
	}{
		{"fails in memory", 16},
		{"fails while spilling", spoolThreshold + 16},
	} {
		t.Run(tt.name, func(t *testing.T) {
			r := io.MultiReader(
				bytes.NewReader(make([]byte, tt.before)),
				failingReader{err: boom},
			)

			_, _, cleanup, err := spool(unsized{r: r})
			require.Error(t, err)
			require.ErrorIs(t, err, boom)
			require.NotNil(t, cleanup, "cleanup is never nil, even on failure")

			cleanup()
		})
	}
}

type failingReader struct{ err error }

func (f failingReader) Read([]byte) (int, error) { return 0, f.err }
