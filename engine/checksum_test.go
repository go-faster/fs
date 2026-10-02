package engine

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/checksum"
	"github.com/go-faster/fs/internal/sse"
)

func digest(t *testing.T, a checksum.Algorithm, data []byte) string {
	t.Helper()

	h, err := a.New()
	require.NoError(t, err)

	_, _ = h.Write(data)

	return checksum.Encode(h.Sum(nil))
}

// backends are the engines the checksum cases run on: one node and three.
func backends(t *testing.T) map[string]fs.Storage {
	t.Helper()

	out := map[string]fs.Storage{
		"single": cluster(t, 1, Config{BlockSize: 4096, InlineLimit: 256})[0].engine,
		"three":  cluster(t, 3, Config{BlockSize: 4096, InlineLimit: 256})[0].engine,
	}
	for _, s := range out {
		require.NoError(t, s.CreateBucket(context.Background(), "b"))
	}

	return out
}

func TestPutChecksum(t *testing.T) {
	data := bytes.Repeat([]byte("checksummed;"), 1000)

	for _, a := range []checksum.Algorithm{checksum.CRC32, checksum.CRC32C, checksum.CRC64NVME, checksum.SHA1, checksum.SHA256} {
		for name, s := range backends(t) {
			ctx := context.Background()

			resp, err := s.PutObject(ctx, &fs.PutObjectRequest{
				Bucket: "b", Key: "k", Reader: bytes.NewReader(data), Size: int64(len(data)),
				ChecksumAlgorithm: string(a), Checksum: digest(t, a, data),
			})
			require.NoError(t, err, "%s %s", name, a)
			assert.Equal(t, digest(t, a, data), resp.Checksum, "%s %s", name, a)

			got, err := s.GetObject(ctx, "b", "k")
			require.NoError(t, err)

			_ = got.Reader.Close()

			assert.Equal(t, string(a), got.ChecksumAlgorithm, name)
			assert.Equal(t, digest(t, a, data), got.Checksum, name)
			assert.Equal(t, string(checksum.FullObject), got.ChecksumType, name)

			// A body that is not what the client says it is is not stored.
			_, err = s.PutObject(ctx, &fs.PutObjectRequest{
				Bucket: "b", Key: "bad", Reader: bytes.NewReader(data), Size: int64(len(data)),
				ChecksumAlgorithm: string(a), Checksum: digest(t, a, []byte("other")),
			})
			require.ErrorIs(t, err, fs.ErrBadDigest, "%s %s", name, a)

			_, err = s.GetObject(ctx, "b", "bad")
			require.ErrorIs(t, err, fs.ErrObjectNotFound, name)
		}
	}
}

func TestMultipartChecksum(t *testing.T) {
	parts := make([][]byte, 3)
	for i := range parts {
		parts[i] = make([]byte, 5000+i*777)
		_, _ = rand.Read(parts[i])
	}

	whole := bytes.Join(parts, nil)

	for _, c := range []struct {
		alg  checksum.Algorithm
		kind checksum.Type
	}{
		{checksum.SHA256, checksum.Composite},
		{checksum.CRC32, checksum.Composite},
		{checksum.CRC64NVME, checksum.FullObject},
		{checksum.CRC32C, checksum.FullObject},
	} {
		got := map[string]string{}

		for name, s := range backends(t) {
			ctx := context.Background()

			up, err := s.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{
				Bucket: "b", Key: "mp", ChecksumAlgorithm: string(c.alg), ChecksumType: string(c.kind),
			})
			require.NoError(t, err)

			var completed []fs.CompletedPart

			for i, data := range parts {
				p, err := s.UploadPart(ctx, &fs.UploadPartRequest{
					Bucket: "b", Key: "mp", UploadID: up.UploadID, PartNumber: i + 1,
					Reader: bytes.NewReader(data), Size: int64(len(data)),
					ChecksumAlgorithm: string(c.alg), Checksum: digest(t, c.alg, data),
				})
				require.NoError(t, err, "%s %s", name, c.alg)
				require.Equal(t, digest(t, c.alg, data), p.Checksum)

				completed = append(completed, fs.CompletedPart{PartNumber: i + 1, ETag: p.ETag, Checksum: p.Checksum})
			}

			done, err := s.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
				Bucket: "b", Key: "mp", UploadID: up.UploadID, Parts: completed,
			})
			require.NoError(t, err, "%s %s", name, c.alg)
			assert.Equal(t, string(c.kind), done.ChecksumType, name)

			got[name] = done.Checksum
		}

		require.NotEmpty(t, got["single"])
		assert.Equal(t, got["single"], got["three"], "%s %s", c.alg, c.kind)

		if c.kind == checksum.FullObject {
			assert.Equal(t, digest(t, c.alg, whole), got["single"], "FULL_OBJECT is the digest of the whole body")
		} else {
			assert.Equal(t, composite(t, c.alg, parts), got["single"], "COMPOSITE is the digest of the part digests")
		}
	}
}

// composite computes what S3 reports for a composite multipart checksum: the
// digest of the parts' digests, suffixed with the part count.
func composite(t *testing.T, a checksum.Algorithm, parts [][]byte) string {
	t.Helper()

	raw := make([][]byte, len(parts))
	for i, p := range parts {
		d, err := a.Decode(digest(t, a, p))
		require.NoError(t, err)

		raw[i] = d
	}

	out, err := checksum.CompositeOf(a, raw)
	require.NoError(t, err)

	return out
}

func TestMultipartChecksumRefusals(t *testing.T) {
	e := newEngine(t, 1)
	ctx := context.Background()
	data := []byte("part data")

	start := func() (*fs.MultipartUpload, *fs.Part) {
		up, err := e.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{Bucket: "b", Key: "k", ChecksumAlgorithm: "SHA256"})
		require.NoError(t, err)

		p, err := e.UploadPart(ctx, &fs.UploadPartRequest{
			Bucket: "b", Key: "k", UploadID: up.UploadID, PartNumber: 1, Reader: bytes.NewReader(data), Size: int64(len(data)),
		})
		require.NoError(t, err)
		require.NotEmpty(t, p.Checksum, "a part of an upload with an algorithm is digested even if it named none")

		return up, p
	}

	up, p := start()
	_, err := e.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket: "b", Key: "k", UploadID: up.UploadID,
		Parts: []fs.CompletedPart{{PartNumber: 1, ETag: p.ETag, Checksum: digest(t, checksum.SHA256, []byte("x"))}},
	})
	require.ErrorIs(t, err, fs.ErrInvalidPart, "a completion naming another part checksum")

	up, p = start()
	_, err = e.CompleteMultipartUpload(ctx, &fs.CompleteMultipartUploadRequest{
		Bucket: "b", Key: "k", UploadID: up.UploadID, Checksum: "bm90IGl0LTE=",
		Parts: []fs.CompletedPart{{PartNumber: 1, ETag: p.ETag}},
	})
	require.ErrorIs(t, err, fs.ErrBadDigest, "a completion claiming another object checksum")

	_, err = e.CreateMultipartUpload(ctx, &fs.CreateMultipartUploadRequest{
		Bucket: "b", Key: "k", ChecksumAlgorithm: "SHA256", ChecksumType: string(checksum.FullObject),
	})
	require.ErrorIs(t, err, fs.ErrInvalidDigest, "SHA-256 cannot be combined into a whole-object digest")

	_, err = e.PutObject(ctx, &fs.PutObjectRequest{
		Bucket: "b", Key: "k", Reader: bytes.NewReader(data), Size: int64(len(data)), ChecksumAlgorithm: "MD4",
	})
	require.ErrorIs(t, err, fs.ErrInvalidDigest)
}

func TestEncryptedChecksumIsOfPlaintext(t *testing.T) {
	e := sealedEngine(t, keyring(t, masterKey(t)))
	data := []byte(fmt.Sprint("plaintext ", 42))

	resp, err := e.PutObject(context.Background(), &fs.PutObjectRequest{
		Bucket: "b", Key: "k", Reader: bytes.NewReader(data), Size: int64(len(data)),
		ServerSideEncryption: sse.Algorithm, ChecksumAlgorithm: "CRC32", Checksum: digest(t, checksum.CRC32, data),
	})
	require.NoError(t, err)
	assert.Equal(t, digest(t, checksum.CRC32, data), resp.Checksum)
}
