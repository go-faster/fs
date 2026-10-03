package engine

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

// The crash test kills a writer mid-flight and reopens what it left: no object
// visible afterwards may be torn, and every write the writer saw acknowledged
// must be there. That is the engine's atomicity claim — a version becomes
// current only when its metadata row is written, after its blocks — tested
// against the one thing that can falsify it.

const crashBucket = "crash"

// crashContent is deterministic, torn-detectable content for object n: a
// marker embedding n, repeated past one block so a write spans several.
func crashContent(n int) []byte {
	return bytes.Repeat(fmt.Appendf(nil, "%08d.", n), 300*1024/9)
}

// TestCrashWorker is the child process: it writes objects with full fsync,
// single PUTs and multipart uploads alike, until its parent kills it. It runs
// only when FS_CRASH_DIR is set.
func TestCrashWorker(t *testing.T) {
	dir := os.Getenv("FS_CRASH_DIR")
	if dir == "" {
		t.Skip("worker process only")
	}

	ctx := context.Background()

	e, err := Open(filepath.Join(dir, "engine"), Options{})
	require.NoError(t, err)
	require.NoError(t, e.CreateBucket(ctx, crashBucket))

	// acked.log records keys whose write returned, synced line by line so the
	// parent can read it after the kill.
	logf, err := os.OpenFile(filepath.Join(dir, "acked.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	require.NoError(t, err)

	for n := 0; ; n++ {
		key := fmt.Sprintf("obj-%d", n)
		content := crashContent(n)

		if n%3 == 2 {
			crashMultipart(t, e, key, content)
		} else {
			_, err := e.PutObject(ctx, &fs.PutObjectRequest{
				Bucket: crashBucket, Key: key, Reader: bytes.NewReader(content), Size: int64(len(content)),
			})
			require.NoError(t, err)
		}

		_, _ = fmt.Fprintln(logf, key)
		_ = logf.Sync()
	}
}

func crashMultipart(t *testing.T, e *Engine, key string, content []byte) {
	t.Helper()

	ctx := context.Background()

	up, err := e.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: crashBucket, Key: key})
	require.NoError(t, err)

	half := len(content) / 2

	var parts []fs.CompletedPart

	for i, chunk := range [][]byte{content[:half], content[half:]} {
		p, err := e.UploadPart(ctx, &fs.UploadPartRequest{
			Bucket: crashBucket, Key: key, UploadID: up.UploadID, PartNumber: i + 1,
			Reader: bytes.NewReader(chunk), Size: int64(len(chunk)),
		})
		require.NoError(t, err)

		parts = append(parts, fs.CompletedPart{PartNumber: i + 1, ETag: p.ETag})
	}

	_, err = e.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket: crashBucket, Key: key, UploadID: up.UploadID, Parts: parts,
	})
	require.NoError(t, err)
}

// TestCrashConsistency kills a writer at varied moments and checks what it
// left.
func TestCrashConsistency(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns writer processes")
	}

	if runtime.GOOS == "windows" {
		t.Skip("kill-and-reopen durability semantics are tested on POSIX")
	}

	for i, jitter := range []time.Duration{1, 8, 20, 45} {
		t.Run(strconv.Itoa(i), func(t *testing.T) {
			dir := t.TempDir()

			cmd := exec.Command(os.Args[0], "-test.run=^TestCrashWorker$", "-test.v") //nolint:gosec // Re-exec of the test binary.
			cmd.Env = append(os.Environ(), "FS_CRASH_DIR="+dir)
			require.NoError(t, cmd.Start())

			// Wait for an acknowledged write, so the kill always lands while
			// the writer is mid-stream, however slow the runner.
			require.Eventually(t, func() bool {
				return len(ackedKeys(t, dir)) > 0
			}, 30*time.Second, 2*time.Millisecond, "writer never acknowledged a write")

			time.Sleep(jitter * time.Millisecond)
			require.NoError(t, cmd.Process.Kill())

			_, _ = cmd.Process.Wait()

			verifyAfterCrash(t, dir)
		})
	}
}

func verifyAfterCrash(t *testing.T, dir string) {
	t.Helper()

	ctx := context.Background()

	e, err := Open(filepath.Join(dir, "engine"), Options{})
	require.NoError(t, err)

	defer func() { _ = e.Close() }()

	list, err := e.ListObjects(ctx, &fs.ListObjectsRequest{Bucket: crashBucket})
	require.NoError(t, err)
	require.NotEmpty(t, list.Objects)

	seen := map[string]bool{}

	for _, o := range list.Objects {
		n, ok := objIndex(o.Key)
		require.True(t, ok, "unexpected key %q", o.Key)

		resp, err := e.GetObject(ctx, crashBucket, o.Key)
		require.NoError(t, err, "a listed object must read")

		got, err := io.ReadAll(resp.Reader)
		_ = resp.Reader.Close()

		require.NoError(t, err, "object %q does not read back", o.Key)
		require.Equal(t, crashContent(n), got, "object %q is torn", o.Key)

		seen[o.Key] = true
	}

	acked := ackedKeys(t, dir)
	for _, key := range acked {
		require.True(t, seen[key], "acknowledged object %q missing after the crash", key)
	}

	t.Logf("after the kill: %d acknowledged, %d visible", len(acked), len(list.Objects))
}

func objIndex(key string) (int, bool) {
	rest, ok := strings.CutPrefix(key, "obj-")
	if !ok {
		return 0, false
	}

	n, err := strconv.Atoi(rest)

	return n, err == nil
}

func ackedKeys(t *testing.T, dir string) []string {
	t.Helper()

	f, err := os.Open(filepath.Join(dir, "acked.log")) //nolint:gosec // test path.
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
