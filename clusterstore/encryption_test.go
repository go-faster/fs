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

// TestClusterMultipartEncrypted covers the path a large object takes on the
// cluster. Part sizes are deliberately not multiples of the chunk size, so a
// completion that concatenated the parts as they were sealed would fail here.
func TestClusterMultipartEncrypted(t *testing.T) {
	for _, s := range []scheme.Scheme{
		{Kind: scheme.RF3},
		{Kind: scheme.EC, K: 2, M: 1},
	} {
		t.Run(s.String(), func(t *testing.T) {
			store, fc := encryptedCluster(t, s, testKeyring(t))
			ctx := t.Context()

			require.NoError(t, store.CreateBucket(ctx, "bucket"))

			up, err := store.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{
				Bucket: "bucket", Key: "big", ServerSideEncryption: sse.Algorithm,
			})
			require.NoError(t, err)

			partBodies := [][]byte{
				bytes.Repeat([]byte("alpha part;"), 7000),
				bytes.Repeat([]byte("beta part;"), 5000),
				[]byte("gamma tail"),
			}

			completed := make([]fs.CompletedPart, 0, len(partBodies))

			for i, body := range partBodies {
				p, err := store.UploadPart(ctx, &fs.UploadPartRequest{
					Bucket: "bucket", Key: "big", UploadID: up.UploadID,
					PartNumber: i + 1, Reader: bytes.NewReader(body), Size: int64(len(body)),
				})
				require.NoError(t, err)
				require.Equal(t, int64(len(body)), p.Size, "a part must report its plaintext size")

				completed = append(completed, fs.CompletedPart{PartNumber: i + 1, ETag: p.ETag})
			}

			// Parts are sealed on arrival: an abandoned upload must not leave
			// readable content behind.
			requireNoPlaintextOnDisk(t, fc, []byte("alpha part"))

			resp, err := store.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
				Bucket: "bucket", Key: "big", UploadID: up.UploadID, Parts: completed,
			})
			require.NoError(t, err)
			require.Equal(t, sse.Algorithm, resp.ServerSideEncryption)

			want := bytes.Join(partBodies, nil)

			got, err := store.GetObject(ctx, "bucket", "big")
			require.NoError(t, err)

			defer func() { _ = got.Reader.Close() }()

			require.Equal(t, int64(len(want)), got.Size, "completed object must report the plaintext size")
			require.Equal(t, sse.Algorithm, got.ServerSideEncryption)

			body, err := io.ReadAll(got.Reader)
			require.NoError(t, err)
			require.Equal(t, want, body)

			requireNoPlaintextOnDisk(t, fc, []byte("beta part"))
		})
	}
}

// requireNoPlaintextOnDisk fails if any fragment on any peer contains needle.
func requireNoPlaintextOnDisk(t *testing.T, fc *fakeCluster, needle []byte) {
	t.Helper()

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
				require.False(t, bytes.Contains(data, needle),
					"fragment %s on %s holds readable plaintext", name, node)
			}
		}
	}
}
