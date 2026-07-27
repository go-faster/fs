package clusterstore

import (
	"bytes"
	"io"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/scheme"
	"github.com/go-faster/fs/internal/sse"
)

func testKeyring(t *testing.T) *sse.Keyring {
	t.Helper()

	key, err := sse.NewKey()
	require.NoError(t, err)

	mk, err := sse.NewMasterKey(key)
	require.NoError(t, err)

	kr, err := sse.NewKeyring(mk)
	require.NoError(t, err)

	return kr
}

// encryptedCluster returns a storage over a fake cluster that can encrypt,
// plus the fake so a test can look at what actually landed on the shards.
func encryptedCluster(t *testing.T, s scheme.Scheme, kr *sse.Keyring) (*Storage, *fakeCluster) {
	t.Helper()

	fc := newFakeCluster(6, 2)

	c, err := New(Config{
		Topology: StaticTopology{T: fc.topo},
		Peers:    fc,
		Scheme:   fixedScheme(s),
		Keyring:  kr,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	return NewStorage(c), fc
}

// TestClusterEncryptedRoundTrip covers every scheme, because encryption sits
// above the fragmenter and must be invisible to all of them.
func TestClusterEncryptedRoundTrip(t *testing.T) {
	for _, s := range []scheme.Scheme{
		{Kind: scheme.RF25},
		{Kind: scheme.RF3},
		{Kind: scheme.EC, K: 2, M: 1},
		{Kind: scheme.EC, K: 4, M: 2},
	} {
		t.Run(s.String(), func(t *testing.T) {
			for _, size := range []int{0, 1, 5000, sse.ChunkSize, sse.ChunkSize + 1, 3*sse.ChunkSize + 77} {
				t.Run(strconv.Itoa(size), func(t *testing.T) {
					store, _ := encryptedCluster(t, s, testKeyring(t))
					ctx := t.Context()

					require.NoError(t, store.CreateBucket(ctx, "bucket"))

					body := bytes.Repeat([]byte("cluster!"), size/8+1)[:size]

					_, err := store.PutObject(ctx, &fs.PutObjectRequest{
						Reader: bytes.NewReader(body), Bucket: "bucket", Key: "obj",
						Size: int64(size), ServerSideEncryption: sse.Algorithm,
					})
					require.NoError(t, err)

					resp, err := store.GetObject(ctx, "bucket", "obj")
					require.NoError(t, err)

					defer func() { _ = resp.Reader.Close() }()

					require.Equal(t, int64(size), resp.Size, "GET must report the plaintext size")
					require.Equal(t, sse.Algorithm, resp.ServerSideEncryption)

					got, err := io.ReadAll(resp.Reader)
					require.NoError(t, err)
					require.Equal(t, body, got)
				})
			}
		})
	}
}

// TestShardsCarryCiphertext is the property the whole placement decision
// exists for: encryption happens above the fragmenter, so no shard on any peer
// holds readable object content.
func TestShardsCarryCiphertext(t *testing.T) {
	store, fc := encryptedCluster(t, scheme.Scheme{Kind: scheme.EC, K: 2, M: 1}, testKeyring(t))
	ctx := t.Context()

	require.NoError(t, store.CreateBucket(ctx, "bucket"))

	body := bytes.Repeat([]byte("the quick brown fox;"), 3000)

	_, err := store.PutObject(ctx, &fs.PutObjectRequest{
		Reader: bytes.NewReader(body), Bucket: "bucket", Key: "obj",
		Size: int64(len(body)), ServerSideEncryption: sse.Algorithm,
	})
	require.NoError(t, err)

	inspected := 0

	for node, store := range fc.stores {
		for _, disk := range fc.disksOf(node) {
			names, err := store.List(t.Context(), disk, "")
			if err != nil {
				continue
			}

			for _, name := range names {
				rc, _, err := store.Open(t.Context(), disk, name)
				if err != nil {
					continue
				}

				data, err := io.ReadAll(rc)
				_ = rc.Close()

				require.NoError(t, err)

				require.False(t, bytes.Contains(data, []byte("quick brown fox")),
					"fragment %s on %s holds readable plaintext", name, node)

				inspected++
			}
		}
	}

	require.NotZero(t, inspected, "the test cluster stored nothing to inspect")
}

// TestClusterReadWithoutKeys: a node that does not hold the master key must
// say so rather than hand back ciphertext as object content.
func TestClusterReadWithoutKeys(t *testing.T) {
	kr := testKeyring(t)
	store, fc := encryptedCluster(t, scheme.Scheme{Kind: scheme.RF3}, kr)
	ctx := t.Context()

	require.NoError(t, store.CreateBucket(ctx, "bucket"))

	_, err := store.PutObject(ctx, &fs.PutObjectRequest{
		Reader: bytes.NewReader([]byte("confidential")), Bucket: "bucket", Key: "obj",
		Size: 12, ServerSideEncryption: sse.Algorithm,
	})
	require.NoError(t, err)

	// The same fragments, read by a coordinator with no keyring.
	bare, err := New(Config{
		Topology: StaticTopology{T: fc.topo},
		Peers:    fc,
		Scheme:   fixedScheme(scheme.Scheme{Kind: scheme.RF3}),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = bare.Close() })

	_, err = NewStorage(bare).GetObject(ctx, "bucket", "obj")
	require.ErrorIs(t, err, fs.ErrUnsupportedOperation)
}

// TestClusterEncryptionWithoutKeyringRefused: asking a cluster with no master
// key to encrypt must fail, never store plaintext under a header saying
// otherwise.
func TestClusterEncryptionWithoutKeyringRefused(t *testing.T) {
	fc := newFakeCluster(6, 2)

	c, err := New(Config{
		Topology: StaticTopology{T: fc.topo},
		Peers:    fc,
		Scheme:   fixedScheme(scheme.Scheme{Kind: scheme.RF3}),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	store := NewStorage(c)
	ctx := t.Context()
	require.NoError(t, store.CreateBucket(ctx, "bucket"))

	_, err = store.PutObject(ctx, &fs.PutObjectRequest{
		Reader: bytes.NewReader([]byte("x")), Bucket: "bucket", Key: "obj", Size: 1,
		ServerSideEncryption: sse.Algorithm,
	})
	require.ErrorIs(t, err, fs.ErrUnsupportedOperation)
}

// TestListingReportsPlaintextSize: Size on the sidecar is the stored length so
// the fragmenter can use it, which makes every client-facing size a place to
// leak one tag per 64 KiB.
func TestListingReportsPlaintextSize(t *testing.T) {
	store, _ := encryptedCluster(t, scheme.Scheme{Kind: scheme.RF3}, testKeyring(t))
	ctx := t.Context()

	require.NoError(t, store.CreateBucket(ctx, "bucket"))

	const size = 2*sse.ChunkSize + 10

	body := bytes.Repeat([]byte("z"), size)

	_, err := store.PutObject(ctx, &fs.PutObjectRequest{
		Reader: bytes.NewReader(body), Bucket: "bucket", Key: "obj",
		Size: size, ServerSideEncryption: sse.Algorithm,
	})
	require.NoError(t, err)

	page, err := store.ListObjects(ctx, &fs.ListObjectsRequest{Bucket: "bucket", Limit: 10})
	require.NoError(t, err)
	require.Len(t, page.Objects, 1)
	require.Equal(t, int64(size), page.Objects[0].Size, "listing leaked the stored size")

	attrs, err := store.ObjectAttributes(ctx, "bucket", "obj")
	require.NoError(t, err)
	require.Equal(t, int64(size), attrs.Size, "attributes leaked the stored size")
}

// disksOf lists the disks the topology gives a node, so a test can walk what
// actually landed on them.
func (f *fakeCluster) disksOf(node cluster.NodeID) []cluster.DiskID {
	var disks []cluster.DiskID

	for _, n := range f.topo.Nodes {
		if n.ID != node {
			continue
		}

		for _, d := range n.Disks {
			disks = append(disks, d.ID)
		}
	}

	return disks
}
