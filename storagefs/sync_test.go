package storagefs

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

func TestParseSyncPolicy(t *testing.T) {
	for in, want := range map[string]SyncPolicy{
		"none": SyncNone, "file": SyncFile, "file+dir": SyncFileDir, "": SyncFileDir,
	} {
		got, err := ParseSyncPolicy(in)
		require.NoError(t, err)
		require.Equal(t, want, got)
	}

	_, err := ParseSyncPolicy("sometimes")
	require.Error(t, err)
}

// TestSyncPolicyRoundTrip verifies every policy produces a correct, readable
// object (durability differences aren't observable without a real crash).
func TestSyncPolicyRoundTrip(t *testing.T) {
	for _, p := range []SyncPolicy{SyncNone, SyncFile, SyncFileDir} {
		s, err := New(t.TempDir(), WithSyncPolicy(p))
		require.NoError(t, err)

		ctx := t.Context()
		require.NoError(t, s.CreateBucket(ctx, "b"))

		content := bytes.Repeat([]byte("durable"), 1000)
		_, err = s.PutObject(ctx, &fs.PutObjectRequest{
			Bucket: "b", Key: "k", Reader: bytes.NewReader(content), Size: int64(len(content)),
		})
		require.NoError(t, err)

		obj, err := s.GetObject(ctx, "b", "k")
		require.NoError(t, err)

		got, err := io.ReadAll(obj.Reader)
		require.NoError(t, err)
		require.NoError(t, obj.Reader.Close())
		require.Equal(t, content, got)
	}
}
