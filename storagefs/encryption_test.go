package storagefs

import (
	"bytes"
	"crypto/md5" //nolint:gosec // Matches the ETag the store computes.
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/sse"
)

func newMasterKey(t *testing.T) sse.MasterKey {
	t.Helper()

	key, err := sse.NewKey()
	require.NoError(t, err)

	mk, err := sse.NewMasterKey(key)
	require.NoError(t, err)

	return mk
}

// encryptedStore builds a store that can encrypt, and returns it with the
// keyring so a test can rotate or drop keys.
func encryptedStore(t *testing.T, mk sse.MasterKey) (s *Storage, root string) {
	t.Helper()

	root = t.TempDir()

	kr, err := sse.NewKeyring(mk)
	require.NoError(t, err)

	s, err = New(root, WithEncryption(kr))
	require.NoError(t, err)

	require.NoError(t, s.CreateBucket(t.Context(), "b"))

	return s, root
}

func putEncrypted(t *testing.T, s *Storage, key string, body []byte) {
	t.Helper()

	_, err := s.PutObject(t.Context(), &fs.PutObjectRequest{
		Reader:               bytes.NewReader(body),
		Bucket:               "b",
		Key:                  key,
		Size:                 int64(len(body)),
		ServerSideEncryption: sse.Algorithm,
	})
	require.NoError(t, err)
}

func readAll(t *testing.T, s *Storage, key string) ([]byte, *fs.GetObjectResponse) {
	t.Helper()

	resp, err := s.GetObject(t.Context(), "b", key)
	require.NoError(t, err)

	defer func() { _ = resp.Reader.Close() }()

	body, err := io.ReadAll(resp.Reader)
	require.NoError(t, err)

	return body, resp
}

// bodyPath is where the body of a key in the test bucket is stored, so a test
// can look at the bytes on disk rather than take the store's word for it.
func bodyPath(root, key string) string {
	return filepath.Join(root, "b", objectRelPath(key))
}

func TestEncryptedRoundTrip(t *testing.T) {
	sizes := []int{0, 1, 1000, sse.ChunkSize - 1, sse.ChunkSize, sse.ChunkSize + 1, 3*sse.ChunkSize + 17}

	for _, size := range sizes {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			s, _ := encryptedStore(t, newMasterKey(t))

			body := bytes.Repeat([]byte("payload!"), size/8+1)[:size]
			putEncrypted(t, s, "k", body)

			got, resp := readAll(t, s, "k")
			require.Equal(t, body, got)
			require.Equal(t, int64(size), resp.Size, "GET must report the plaintext size")
			require.Equal(t, sse.Algorithm, resp.ServerSideEncryption)

			sum := md5.Sum(body) //nolint:gosec // Matches the ETag the store computes.
			require.Equal(t, hex.EncodeToString(sum[:]), resp.ETag,
				"the ETag must stay the plaintext MD5")
		})
	}
}

// TestBodyOnDiskIsCiphertext is the whole point of the feature: a disk that
// leaves the operator's custody must not carry readable object content.
func TestBodyOnDiskIsCiphertext(t *testing.T) {
	s, root := encryptedStore(t, newMasterKey(t))

	body := bytes.Repeat([]byte("the quick brown fox;"), 4000)
	putEncrypted(t, s, "k", body)

	stored, err := os.ReadFile(bodyPath(root, "k"))
	require.NoError(t, err)

	require.False(t, bytes.Contains(stored, []byte("quick brown fox")),
		"plaintext is readable in the stored file")
	require.False(t, bytes.Contains(stored, body[:64]))
	require.Equal(t, sse.CipherSize(int64(len(body))), int64(len(stored)),
		"stored size must be the plaintext plus one tag per chunk")
}

// TestUnencryptedStillPlaintext pins that encryption is opt-in per request:
// without the header the body is stored as-is, so enabling a keyring does not
// silently change what every write does.
func TestUnencryptedStillPlaintext(t *testing.T) {
	s, root := encryptedStore(t, newMasterKey(t))

	body := []byte("stored in the clear")

	_, err := s.PutObject(t.Context(), &fs.PutObjectRequest{
		Reader: bytes.NewReader(body), Bucket: "b", Key: "k", Size: int64(len(body)),
	})
	require.NoError(t, err)

	stored, err := os.ReadFile(bodyPath(root, "k"))
	require.NoError(t, err)
	require.Equal(t, body, stored)

	got, resp := readAll(t, s, "k")
	require.Equal(t, body, got)
	require.Empty(t, resp.ServerSideEncryption)
}

// TestRangeReadsDecrypt is what the chunked format exists for: reading a
// window must not decrypt from byte zero, and must return the right bytes.
func TestRangeReadsDecrypt(t *testing.T) {
	s, _ := encryptedStore(t, newMasterKey(t))

	const size = 3*sse.ChunkSize + 1234

	body := make([]byte, size)
	for i := range body {
		body[i] = byte(i * 7)
	}

	putEncrypted(t, s, "k", body)

	resp, err := s.GetObject(t.Context(), "b", "k")
	require.NoError(t, err)

	defer func() { _ = resp.Reader.Close() }()

	// The read path hands this to http.ServeContent, which requires a seeker.
	rs, ok := resp.Reader.(io.ReadSeeker)
	require.True(t, ok, "an encrypted body must still be seekable, or Range breaks")

	for _, w := range []struct{ off, n int64 }{
		{0, 10},
		{sse.ChunkSize - 5, 10},
		{sse.ChunkSize, 1},
		{2*sse.ChunkSize + 100, 5000},
		{size - 1, 1},
	} {
		_, err := rs.Seek(w.off, io.SeekStart)
		require.NoError(t, err)

		buf := make([]byte, w.n)
		_, err = io.ReadFull(rs, buf)
		require.NoError(t, err)
		require.Equal(t, body[w.off:w.off+w.n], buf, "window at %d", w.off)
	}
}

// TestEncryptionRequestedWithoutKeyring: a store with no master key must
// refuse the write, never quietly store plaintext under a header that claims
// otherwise.
func TestEncryptionRequestedWithoutKeyring(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)
	require.NoError(t, s.CreateBucket(t.Context(), "b"))

	_, err = s.PutObject(t.Context(), &fs.PutObjectRequest{
		Reader: bytes.NewReader([]byte("x")), Bucket: "b", Key: "k", Size: 1,
		ServerSideEncryption: sse.Algorithm,
	})
	require.ErrorIs(t, err, fs.ErrUnsupportedOperation)

	_, err = s.GetObject(t.Context(), "b", "k")
	require.ErrorIs(t, err, fs.ErrObjectNotFound, "the refused write must not have created an object")
}

func TestUnknownAlgorithmRefused(t *testing.T) {
	s, _ := encryptedStore(t, newMasterKey(t))

	_, err := s.PutObject(t.Context(), &fs.PutObjectRequest{
		Reader: bytes.NewReader([]byte("x")), Bucket: "b", Key: "k", Size: 1,
		ServerSideEncryption: "aws:kms",
	})
	require.ErrorIs(t, err, fs.ErrUnsupportedOperation)
}

// TestReadWithoutMasterKey covers the disk-in-the-wrong-hands case from the
// other side: the bytes must be unreadable, and the server must say why rather
// than serve ciphertext.
func TestReadWithoutMasterKey(t *testing.T) {
	mk := newMasterKey(t)
	s, root := encryptedStore(t, mk)

	putEncrypted(t, s, "k", []byte("confidential"))

	// Reopen the same store with no keyring at all.
	bare, err := New(root)
	require.NoError(t, err)

	_, err = bare.GetObject(t.Context(), "b", "k")
	require.ErrorIs(t, err, fs.ErrUnsupportedOperation)

	// And with the wrong master key: the wrap must not open.
	other, err := sse.NewKeyring(newMasterKey(t))
	require.NoError(t, err)

	wrong, err := New(root, WithEncryption(other))
	require.NoError(t, err)

	_, err = wrong.GetObject(t.Context(), "b", "k")
	require.Error(t, err)
	require.NotErrorIs(t, err, fs.ErrObjectNotFound, "an unreadable object is not a missing one")
}

// TestCorruptedCiphertextRefused: the AEAD tag must stop damaged bytes from
// being served, whether or not verify-on-read is enabled.
func TestCorruptedCiphertextRefused(t *testing.T) {
	s, root := encryptedStore(t, newMasterKey(t))

	body := bytes.Repeat([]byte("z"), 5000)
	putEncrypted(t, s, "k", body)

	path := bodyPath(root, "k")

	stored, err := os.ReadFile(path)
	require.NoError(t, err)

	stored[100] ^= 0x01
	require.NoError(t, os.WriteFile(path, stored, 0o600))

	resp, err := s.GetObject(t.Context(), "b", "k")
	require.NoError(t, err, "the damage is in the body, so opening still succeeds")

	defer func() { _ = resp.Reader.Close() }()

	_, err = io.ReadAll(resp.Reader)
	require.Error(t, err, "corrupted ciphertext was served as object content")
}

// TestScrubVerifiesCiphertextWithoutKeys is why the ciphertext checksum
// exists: a scrub must be able to detect bit-rot on a store whose master key
// is not loaded, and must not report every encrypted object as corrupt.
func TestScrubVerifiesCiphertextWithoutKeys(t *testing.T) {
	mk := newMasterKey(t)
	s, root := encryptedStore(t, mk)

	putEncrypted(t, s, "healthy", bytes.Repeat([]byte("a"), 9000))
	putEncrypted(t, s, "rotten", bytes.Repeat([]byte("b"), 9000))

	// A store with no keys at all, as a scrubber would run.
	bare, err := New(root)
	require.NoError(t, err)

	report, err := bare.Scrub(t.Context(), ScrubOptions{})
	require.NoError(t, err)
	require.Empty(t, report.Corrupt, "encrypted objects were reported corrupt by a keyless scrub")
	require.Equal(t, 2, report.OK)

	// Now rot one of them on disk.
	path := bodyPath(root, "rotten")

	stored, err := os.ReadFile(path)
	require.NoError(t, err)

	stored[50] ^= 0xFF
	require.NoError(t, os.WriteFile(path, stored, 0o600))

	report, err = bare.Scrub(t.Context(), ScrubOptions{})
	require.NoError(t, err)
	require.Len(t, report.Corrupt, 1)
	require.Equal(t, "rotten", report.Corrupt[0].Key)
}

// TestVerifyOnReadAcceptsEncrypted guards the combination that would otherwise
// report every encrypted object as damaged: verify-on-read compares the file
// against the checksum of the bytes actually on disk.
func TestVerifyOnReadAcceptsEncrypted(t *testing.T) {
	kr, err := sse.NewKeyring(newMasterKey(t))
	require.NoError(t, err)

	root := t.TempDir()

	s, err := New(root, WithEncryption(kr), WithVerifyReads(true))
	require.NoError(t, err)
	require.NoError(t, s.CreateBucket(t.Context(), "b"))

	body := bytes.Repeat([]byte("verified"), 2000)
	putEncrypted(t, s, "k", body)

	got, _ := readAll(t, s, "k")
	require.Equal(t, body, got)
}

// TestRotationKeepsObjectsReadable is the operator-facing promise: a new
// master key does not strand what was written under the old one.
func TestRotationKeepsObjectsReadable(t *testing.T) {
	oldKey := newMasterKey(t)
	s, root := encryptedStore(t, oldKey)

	body := bytes.Repeat([]byte("before rotation"), 500)
	putEncrypted(t, s, "k", body)

	newKey := newMasterKey(t)

	rotated, err := sse.NewKeyring(newKey, oldKey)
	require.NoError(t, err)

	// Same root, so the bucket and the object are already there.
	after, err := New(root, WithEncryption(rotated))
	require.NoError(t, err)

	resp, err := after.GetObject(t.Context(), "b", "k")
	require.NoError(t, err)

	defer func() { _ = resp.Reader.Close() }()

	got, err := io.ReadAll(resp.Reader)
	require.NoError(t, err)
	require.Equal(t, body, got, "an object written before the rotation became unreadable")

	// New writes go under the new key; the old one is still only needed for
	// the old object.
	putEncrypted(t, after, "fresh", []byte("after rotation"))

	sc, err := after.readSidecar("b", "fresh")
	require.NoError(t, err)
	require.Equal(t, newKey.ID, sc.Encryption.Key.KeyID)

	old, err := after.readSidecar("b", "k")
	require.NoError(t, err)
	require.Equal(t, oldKey.ID, old.Encryption.Key.KeyID)
}

// TestEachObjectHasItsOwnKey: a compromised data key must not open any other
// object.
func TestEachObjectHasItsOwnKey(t *testing.T) {
	s, _ := encryptedStore(t, newMasterKey(t))

	putEncrypted(t, s, "one", []byte("first"))
	putEncrypted(t, s, "two", []byte("second"))

	a, err := s.readSidecar("b", "one")
	require.NoError(t, err)

	b, err := s.readSidecar("b", "two")
	require.NoError(t, err)

	require.NotEqual(t, a.Encryption.Key.Ciphertext, b.Encryption.Key.Ciphertext)
	require.NotEqual(t, a.Encryption.NonceBase, b.Encryption.NonceBase)
}

// TestMultipartEncrypted covers the path a large object actually takes. The
// completed object must be ciphertext, readable, and seekable.
func TestMultipartEncrypted(t *testing.T) {
	s, root := encryptedStore(t, newMasterKey(t))

	up, err := s.CreateMultipartUpload(t.Context(), &fs.CreateMultipartUploadRequest{
		Bucket: "b", Key: "big", ServerSideEncryption: sse.Algorithm,
	})
	require.NoError(t, err)

	// Part sizes deliberately not multiples of the chunk size, so completion
	// cannot get away with concatenating the parts as they were sealed.
	partBodies := [][]byte{
		bytes.Repeat([]byte("first part;"), 9000),
		bytes.Repeat([]byte("second part;"), 7000),
		[]byte("third and last"),
	}

	completed := make([]fs.CompletedPart, 0, len(partBodies))

	for i, body := range partBodies {
		p, err := s.UploadPart(t.Context(), &fs.UploadPartRequest{
			Bucket: "b", Key: "big", UploadID: up.UploadID,
			PartNumber: i + 1, Reader: bytes.NewReader(body), Size: int64(len(body)),
		})
		require.NoError(t, err)

		sum := md5.Sum(body) //nolint:gosec // Matches the ETag the store computes.
		require.Equal(t, hex.EncodeToString(sum[:]), p.ETag,
			"a part's ETag must be the MD5 of its plaintext")
		require.Equal(t, int64(len(body)), p.Size, "a part must report its plaintext size")

		completed = append(completed, fs.CompletedPart{PartNumber: i + 1, ETag: p.ETag})
	}

	resp, err := s.CompleteMultipartUpload(t.Context(), &fs.CompleteMultipartUploadRequest{
		Bucket: "b", Key: "big", UploadID: up.UploadID, Parts: completed,
	})
	require.NoError(t, err)
	require.Equal(t, sse.Algorithm, resp.ServerSideEncryption)

	want := bytes.Join(partBodies, nil)

	got, getResp := readAll(t, s, "big")
	require.Equal(t, want, got)
	require.Equal(t, int64(len(want)), getResp.Size)
	require.Equal(t, sse.Algorithm, getResp.ServerSideEncryption)

	stored, err := os.ReadFile(bodyPath(root, "big"))
	require.NoError(t, err)
	require.False(t, bytes.Contains(stored, []byte("second part")),
		"the completed object holds plaintext")
	require.Equal(t, sse.CipherSize(int64(len(want))), int64(len(stored)),
		"the completed object must be one uniform chunk stream")

	// And it must still seek, which is what re-sealing at completion buys.
	head, err := s.GetObject(t.Context(), "b", "big")
	require.NoError(t, err)

	defer func() { _ = head.Reader.Close() }()

	rs, ok := head.Reader.(io.ReadSeeker)
	require.True(t, ok)

	_, err = rs.Seek(int64(len(want))-20, io.SeekStart)
	require.NoError(t, err)

	tail, err := io.ReadAll(rs)
	require.NoError(t, err)
	require.Equal(t, want[len(want)-20:], tail)
}

// TestMultipartPartsStagedEncrypted: an abandoned upload leaves its parts on
// disk, so the parts themselves must be ciphertext — not just the object they
// would have become.
func TestMultipartPartsStagedEncrypted(t *testing.T) {
	s, _ := encryptedStore(t, newMasterKey(t))

	up, err := s.CreateMultipartUpload(t.Context(), &fs.CreateMultipartUploadRequest{
		Bucket: "b", Key: "big", ServerSideEncryption: sse.Algorithm,
	})
	require.NoError(t, err)

	body := bytes.Repeat([]byte("staged secret;"), 5000)

	_, err = s.UploadPart(t.Context(), &fs.UploadPartRequest{
		Bucket: "b", Key: "big", UploadID: up.UploadID,
		PartNumber: 1, Reader: bytes.NewReader(body), Size: int64(len(body)),
	})
	require.NoError(t, err)

	// The upload is never completed; the part stays in staging.
	partPath := filepath.Join(s.multipart.uploadPath(up.UploadID), "1")

	staged, err := os.ReadFile(partPath)
	require.NoError(t, err)
	require.Equal(t, sse.CipherSize(int64(len(body))), int64(len(staged)))
	require.False(t, bytes.Contains(staged, []byte("staged secret")),
		"an in-progress part is readable plaintext on disk")
}

// TestRotateKeysRewrapsWithoutTouchingBodies is the drill an operator runs:
// after a rotation every object is on the new key, and not one body was
// rewritten.
func TestRotateKeysRewrapsWithoutTouchingBodies(t *testing.T) {
	oldKey := newMasterKey(t)
	s, root := encryptedStore(t, oldKey)

	bodies := map[string][]byte{
		"a": bytes.Repeat([]byte("first;"), 3000),
		"b": bytes.Repeat([]byte("second;"), 100),
		"c": {},
	}
	for k, v := range bodies {
		putEncrypted(t, s, k, v)
	}

	// One object deliberately left in the clear: a rotation must not touch it.
	plain := []byte("never encrypted")
	_, err := s.PutObject(t.Context(), &fs.PutObjectRequest{
		Reader: bytes.NewReader(plain), Bucket: "b", Key: "plain", Size: int64(len(plain)),
	})
	require.NoError(t, err)

	before := map[string][]byte{}

	for k := range bodies {
		stored, err := os.ReadFile(bodyPath(root, k))
		require.NoError(t, err)

		before[k] = stored
	}

	newKey := newMasterKey(t)

	rotated, err := sse.NewKeyring(newKey, oldKey)
	require.NoError(t, err)

	after, err := New(root, WithEncryption(rotated))
	require.NoError(t, err)

	report, err := after.RotateKeys(t.Context())
	require.NoError(t, err)
	require.True(t, report.Done(), "rotation reported failures: %v", report.Failed)
	require.Equal(t, 3, report.Rewrapped)
	require.Equal(t, 1, report.Unencrypted)
	require.Equal(t, 4, report.Scanned)

	for k, want := range bodies {
		// The body on disk is byte-for-byte what it was: rotation is a
		// sidecar-only walk.
		stored, err := os.ReadFile(bodyPath(root, k))
		require.NoError(t, err)
		require.Equal(t, before[k], stored, "rotation rewrote the body of %q", k)

		sc, err := after.readSidecar("b", k)
		require.NoError(t, err)
		require.Equal(t, newKey.ID, sc.Encryption.Key.KeyID)

		got, _ := readAll(t, after, k)
		require.Equal(t, want, got)
	}

	// Now the retired key can go, and everything still reads.
	only, err := sse.NewKeyring(newKey)
	require.NoError(t, err)

	final, err := New(root, WithEncryption(only))
	require.NoError(t, err)

	for k, want := range bodies {
		got, _ := readAll(t, final, k)
		require.Equal(t, want, got)
	}

	// A second pass has nothing left to do, which is how an operator knows the
	// retired key is safe to remove.
	again, err := final.RotateKeys(t.Context())
	require.NoError(t, err)
	require.Zero(t, again.Rewrapped)
	require.Equal(t, 3, again.AlreadyCurrent)
	require.True(t, again.Done())
}

// TestRotateKeysReportsUnreachableObjects: dropping the retired key before the
// rotation finishes must be reported per object, not silently skipped — that
// report is what stops an operator deleting a key they still need.
func TestRotateKeysReportsUnreachableObjects(t *testing.T) {
	oldKey := newMasterKey(t)
	s, root := encryptedStore(t, oldKey)

	putEncrypted(t, s, "stranded", []byte("written under the old key"))

	// A keyring with the new key only: the old wrap cannot be opened.
	only, err := sse.NewKeyring(newMasterKey(t))
	require.NoError(t, err)

	after, err := New(root, WithEncryption(only))
	require.NoError(t, err)

	report, err := after.RotateKeys(t.Context())
	require.NoError(t, err)
	require.False(t, report.Done())
	require.Len(t, report.Failed, 1)
	require.Equal(t, "stranded", report.Failed[0].Key)
}

// TestRotateKeysWithoutKeyring: there is nothing to rotate onto, and saying so
// beats reporting a clean pass over a store it never examined.
func TestRotateKeysWithoutKeyring(t *testing.T) {
	s, err := New(t.TempDir())
	require.NoError(t, err)

	_, err = s.RotateKeys(t.Context())
	require.ErrorIs(t, err, fs.ErrUnsupportedOperation)
}

// TestEncryptedObjectSurvivesCrash covers the crash-consistency half of the
// acceptance: a write interrupted before its sidecar lands must leave either
// nothing or a readable object, never a body no key can open.
func TestEncryptedObjectSurvivesCrash(t *testing.T) {
	mk := newMasterKey(t)
	s, root := encryptedStore(t, mk)

	body := bytes.Repeat([]byte("durable;"), 5000)
	putEncrypted(t, s, "committed", body)

	// Reopen the store as a restart would, with the same keys.
	kr, err := sse.NewKeyring(mk)
	require.NoError(t, err)

	reopened, err := New(root, WithEncryption(kr))
	require.NoError(t, err)

	got, resp := readAll(t, reopened, "committed")
	require.Equal(t, body, got)
	require.Equal(t, sse.Algorithm, resp.ServerSideEncryption)

	// A staged body with no sidecar is what a crash mid-write leaves. It must
	// not become a visible object, and must not disturb the scrub.
	staging := filepath.Join(root, ".tmp")
	require.NoError(t, os.WriteFile(filepath.Join(staging, "obj-orphan"), []byte("half-written"), 0o600))

	report, err := reopened.Scrub(t.Context(), ScrubOptions{})
	require.NoError(t, err)
	require.Empty(t, report.Corrupt)
	require.Equal(t, 1, report.OK, "the orphaned staging file was counted as an object")
}
