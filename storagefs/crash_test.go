package storagefs

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

// crashContent returns deterministic, torn-detectable content for object n: a
// repeated marker embedding n, ~128 KiB, so a partial write is caught by a
// length or byte mismatch.
func crashContent(n int) []byte {
	return bytes.Repeat([]byte(fmt.Sprintf("%08d.", n)), 16*1024)
}

const crashBucket = "crash"

// TestCrashWorker is the child process: it writes objects forever (mixing the
// single-PUT and multipart-complete paths) with full fsync durability, until
// its parent SIGKILLs it. It runs only when FS_CRASH_DIR is set.
func TestCrashWorker(t *testing.T) {
	dir := os.Getenv("FS_CRASH_DIR")
	if dir == "" {
		t.Skip("worker process only")
	}

	ctx := context.Background()

	s, err := New(dir, WithSyncPolicy(SyncFileDir))
	require.NoError(t, err)
	require.NoError(t, s.CreateBucket(ctx, crashBucket))

	// acked.log records keys whose write returned, one per line, flushed so the
	// parent can read it after the kill.
	logf, err := os.OpenFile(filepath.Join(dir, "acked.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	require.NoError(t, err)

	for n := 0; ; n++ {
		key := fmt.Sprintf("obj-%d", n)
		content := crashContent(n)

		if n%5 == 4 {
			writeMultipart(t, s, key, content)
		} else {
			_, err := s.PutObject(ctx, &fs.PutObjectRequest{
				Bucket: crashBucket, Key: key,
				Reader: bytes.NewReader(content), Size: int64(len(content)),
			})
			require.NoError(t, err)
		}

		_, _ = fmt.Fprintln(logf, key)
		_ = logf.Sync()
	}
}

func writeMultipart(t *testing.T, s *Storage, key string, content []byte) {
	t.Helper()

	ctx := context.Background()

	up, err := s.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: crashBucket, Key: key})
	require.NoError(t, err)

	part, err := s.UploadPart(ctx, &fs.UploadPartRequest{
		Bucket: crashBucket, Key: key, UploadID: up.UploadID, PartNumber: 1,
		Reader: bytes.NewReader(content), Size: int64(len(content)),
	})
	require.NoError(t, err)

	_, err = s.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket: crashBucket, Key: key, UploadID: up.UploadID,
		Parts: []fs.CompletedPart{{PartNumber: 1, ETag: part.ETag}},
	})
	require.NoError(t, err)
}

// TestCrashConsistency SIGKILLs a writer mid-flight, several times at varied
// moments, and verifies the invariant: no object visible in the reopened store
// is ever torn, and every acknowledged write is present and intact.
func TestCrashConsistency(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGKILL / fsync crash semantics are POSIX-specific")
	}

	// Vary a small jitter so the kill lands at different points of a write once
	// the writer is warmed up. We first wait for at least one committed object,
	// so the kill is always mid-stream regardless of runner speed — a fixed
	// delay from start flakes on slow CI where the child has not committed
	// anything yet.
	for i, jitter := range []time.Duration{1, 8, 20, 45} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			dir := t.TempDir()

			cmd := exec.Command(os.Args[0], "-test.run=TestCrashWorker", "-test.v") //nolint:gosec // Re-exec of the test binary.

			cmd.Env = append(os.Environ(), "FS_CRASH_DIR="+dir)
			require.NoError(t, cmd.Start())

			// Wait until the writer has committed at least one object.
			bucketDir := filepath.Join(dir, crashBucket)

			require.Eventually(t, func() bool {
				entries, _ := os.ReadDir(bucketDir)
				return len(entries) > 0
			}, 15*time.Second, 2*time.Millisecond, "writer never committed an object")

			time.Sleep(jitter * time.Millisecond)
			require.NoError(t, cmd.Process.Kill()) // SIGKILL
			_, _ = cmd.Process.Wait()

			verifyNoTornObjects(t, dir)
		})
	}
}

func verifyNoTornObjects(t *testing.T, dir string) {
	t.Helper()

	ctx := context.Background()

	s, err := New(dir)
	require.NoError(t, err)

	objects, err := s.ListObjects(ctx, crashBucket, "")
	require.NoError(t, err)
	require.NotEmpty(t, objects, "writer should have committed at least one object before the kill")

	// Every object visible in the listing must read back exactly — never torn.
	seen := make(map[string]struct{}, len(objects))

	for _, o := range objects {
		n, ok := parseObjIndex(o.Key)
		require.True(t, ok, "unexpected key %q (a staging temp file must never appear as an object)", o.Key)

		require.Equal(t, crashContent(n), readObjectContent(t, s, o.Key),
			"object %q is torn", o.Key)

		seen[o.Key] = struct{}{}
	}

	// Every acknowledged write must be present (a returned PutObject/Complete is
	// durable across the kill).
	for _, key := range readAckedKeys(t, dir) {
		_, ok := seen[key]
		require.True(t, ok, "acknowledged object %q missing after crash", key)
	}
}

func parseObjIndex(key string) (int, bool) {
	rest, ok := strings.CutPrefix(key, "obj-")
	if !ok {
		return 0, false
	}

	n, err := strconv.Atoi(rest)

	return n, err == nil
}

func readObjectContent(t *testing.T, s *Storage, key string) []byte {
	t.Helper()

	resp, err := s.GetObject(context.Background(), crashBucket, key)
	require.NoError(t, err)

	defer func() { _ = resp.Reader.Close() }()

	data, err := io.ReadAll(resp.Reader)
	require.NoError(t, err)

	return data
}

func readAckedKeys(t *testing.T, dir string) []string {
	t.Helper()

	f, err := os.Open(filepath.Join(dir, "acked.log"))
	if os.IsNotExist(err) {
		return nil
	}

	require.NoError(t, err)

	defer func() { _ = f.Close() }()

	var keys []string

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			keys = append(keys, line)
		}
	}

	return keys
}
