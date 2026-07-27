package sse_test

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"io"
	"math/big"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/sse"
)

// sizes spans the boundaries the chunking can get wrong: empty, the first
// byte, either side of a chunk edge, and several whole chunks.
var sizes = []int64{
	0, 1, 2, 1023,
	sse.ChunkSize - 1, sse.ChunkSize, sse.ChunkSize + 1,
	2*sse.ChunkSize - 1, 2 * sse.ChunkSize, 2*sse.ChunkSize + 1,
	5*sse.ChunkSize + 12345,
}

// newCipher builds a cipher for a single PUT (part 0); the multipart tests
// construct their own so the part number is visible at the call site.
func newCipher(t *testing.T) (c *sse.Cipher, key, nonceBase []byte) {
	t.Helper()

	key, err := sse.NewKey()
	require.NoError(t, err)

	base, err := sse.NewNonceBase()
	require.NoError(t, err)

	c, err = sse.New(key, base, 0)
	require.NoError(t, err)

	return c, key, base
}

func randomBytes(t *testing.T, n int64) []byte {
	t.Helper()

	b := make([]byte, n)
	_, err := rand.Read(b)
	require.NoError(t, err)

	return b
}

// encrypt seals plain and returns the ciphertext.
func encrypt(t *testing.T, c *sse.Cipher, plain []byte) []byte {
	t.Helper()

	var buf bytes.Buffer

	w := sse.NewWriter(&buf, c)
	n, err := w.Write(plain)
	require.NoError(t, err)
	require.Equal(t, len(plain), n)
	require.NoError(t, w.Close())

	return buf.Bytes()
}

func TestRoundTrip(t *testing.T) {
	for _, size := range sizes {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			c, key, base := newCipher(t)
			plain := randomBytes(t, size)
			ct := encrypt(t, c, plain)

			require.Equal(t, sse.CipherSize(size), int64(len(ct)),
				"stored size must match what CipherSize promises")

			// A fresh Cipher, as a real read would use: the write-side one is
			// not carried over.
			rc, err := sse.New(key, base, 0)
			require.NoError(t, err)

			got, err := io.ReadAll(sse.NewReader(bytes.NewReader(ct), rc, size))
			require.NoError(t, err)
			require.Equal(t, plain, got)
		})
	}
}

// TestCiphertextIsNotPlaintext guards against the failure that would make
// every other test here pass while encrypting nothing.
func TestCiphertextIsNotPlaintext(t *testing.T) {
	c, _, _ := newCipher(t)
	plain := bytes.Repeat([]byte("secret payload;"), 5000)
	ct := encrypt(t, c, plain)

	require.NotContains(t, string(ct), "secret payload")
	require.False(t, bytes.Contains(ct, plain[:64]))
}

// TestWriteInArbitraryPieces covers the buffering: the chunk boundary must
// depend on how many bytes have been written, never on how they were split
// across Write calls.
func TestWriteInArbitraryPieces(t *testing.T) {
	const size = 3*sse.ChunkSize + 777

	_, key, base := newCipher(t)
	plain := randomBytes(t, size)

	for _, step := range []int{1, 7, 4096, sse.ChunkSize - 1, sse.ChunkSize, sse.ChunkSize + 1, size} {
		t.Run(fmt.Sprint(step), func(t *testing.T) {
			wc, err := sse.New(key, base, 0)
			require.NoError(t, err)

			var buf bytes.Buffer

			w := sse.NewWriter(&buf, wc)

			for off := 0; off < len(plain); off += step {
				end := min(off+step, len(plain))

				n, err := w.Write(plain[off:end])
				require.NoError(t, err)
				require.Equal(t, end-off, n)
			}

			require.NoError(t, w.Close())

			rc, err := sse.New(key, base, 0)
			require.NoError(t, err)

			got, err := io.ReadAll(sse.NewReader(bytes.NewReader(buf.Bytes()), rc, size))
			require.NoError(t, err)
			require.Equal(t, plain, got)
		})
	}
}

// TestSeek is the property a range GET depends on: reading from any plaintext
// offset yields the same bytes as the plaintext from that offset.
func TestSeek(t *testing.T) {
	const size = 4*sse.ChunkSize + 4242

	c, key, base := newCipher(t)
	plain := randomBytes(t, size)
	ct := encrypt(t, c, plain)

	offsets := []int64{
		0, 1, 100,
		sse.ChunkSize - 1, sse.ChunkSize, sse.ChunkSize + 1,
		3 * sse.ChunkSize, size - 1, size,
	}

	for _, off := range offsets {
		t.Run(fmt.Sprint(off), func(t *testing.T) {
			rc, err := sse.New(key, base, 0)
			require.NoError(t, err)

			r := sse.NewReader(bytes.NewReader(ct), rc, size)

			pos, err := r.Seek(off, io.SeekStart)
			require.NoError(t, err)
			require.Equal(t, off, pos)

			got, err := io.ReadAll(r)
			require.NoError(t, err)
			require.Equal(t, plain[off:], got)
		})
	}
}

func TestSeekWhence(t *testing.T) {
	const size = 2*sse.ChunkSize + 10

	c, key, base := newCipher(t)
	plain := randomBytes(t, size)
	ct := encrypt(t, c, plain)

	rc, err := sse.New(key, base, 0)
	require.NoError(t, err)

	r := sse.NewReader(bytes.NewReader(ct), rc, size)

	// SeekEnd with offset 0 is how http.ServeContent asks for the size.
	end, err := r.Seek(0, io.SeekEnd)
	require.NoError(t, err)
	require.Equal(t, int64(size), end)

	_, err = r.Seek(0, io.SeekStart)
	require.NoError(t, err)

	buf := make([]byte, 100)
	_, err = io.ReadFull(r, buf)
	require.NoError(t, err)

	cur, err := r.Seek(50, io.SeekCurrent)
	require.NoError(t, err)
	require.Equal(t, int64(150), cur)

	rest := make([]byte, 20)
	_, err = io.ReadFull(r, rest)
	require.NoError(t, err)
	require.Equal(t, plain[150:170], rest)

	back, err := r.Seek(-10, io.SeekEnd)
	require.NoError(t, err)
	require.Equal(t, int64(size-10), back)

	tail, err := io.ReadAll(r)
	require.NoError(t, err)
	require.Equal(t, plain[size-10:], tail)

	_, err = r.Seek(-1, io.SeekStart)
	require.Error(t, err, "seeking before the start is an error, not a clamp")
}

// TestSeekPastEndReadsNothing pins the io.Reader contract at and beyond EOF.
func TestSeekPastEndReadsNothing(t *testing.T) {
	const size = 1000

	c, key, base := newCipher(t)
	ct := encrypt(t, c, randomBytes(t, size))

	rc, err := sse.New(key, base, 0)
	require.NoError(t, err)

	r := sse.NewReader(bytes.NewReader(ct), rc, size)

	_, err = r.Seek(size+500, io.SeekStart)
	require.NoError(t, err)

	n, err := r.Read(make([]byte, 10))
	require.Equal(t, 0, n)
	require.ErrorIs(t, err, io.EOF)
}

// TestTamperDetected is the reason for an AEAD rather than a raw cipher: a
// flipped bit anywhere must be refused, not served.
func TestTamperDetected(t *testing.T) {
	const size = 2*sse.ChunkSize + 500

	c, key, base := newCipher(t)
	plain := randomBytes(t, size)
	ct := encrypt(t, c, plain)

	// One position inside each chunk's body, and one inside each chunk's tag.
	positions := []int{
		0, 100,
		sse.ChunkSize, sse.ChunkSize + sse.TagSize - 1,
		sse.ChunkSize + sse.TagSize + 5,
		len(ct) - 1,
	}

	for _, pos := range positions {
		t.Run(fmt.Sprint(pos), func(t *testing.T) {
			damaged := bytes.Clone(ct)
			damaged[pos] ^= 0x01

			rc, err := sse.New(key, base, 0)
			require.NoError(t, err)

			_, err = io.ReadAll(sse.NewReader(bytes.NewReader(damaged), rc, size))
			require.Error(t, err, "a flipped bit at %d was served as if it were the object", pos)
		})
	}
}

// TestChunkSwapDetected covers the attack per-chunk tags alone would miss if
// the nonce did not depend on the chunk index.
func TestChunkSwapDetected(t *testing.T) {
	const size = 3 * sse.ChunkSize

	c, key, base := newCipher(t)
	ct := encrypt(t, c, randomBytes(t, size))

	const stored = sse.ChunkSize + sse.TagSize

	swapped := bytes.Clone(ct)
	copy(swapped[0:stored], ct[stored:2*stored])
	copy(swapped[stored:2*stored], ct[0:stored])

	rc, err := sse.New(key, base, 0)
	require.NoError(t, err)

	_, err = io.ReadAll(sse.NewReader(bytes.NewReader(swapped), rc, size))
	require.Error(t, err, "reordered chunks were served as the object")
}

// TestTruncationDetected is why the reader is told the plaintext size instead
// of inferring it: a missing trailing chunk leaves every remaining tag valid.
func TestTruncationDetected(t *testing.T) {
	const size = 3 * sse.ChunkSize

	c, key, base := newCipher(t)
	ct := encrypt(t, c, randomBytes(t, size))

	truncated := ct[:len(ct)-(sse.ChunkSize+sse.TagSize)]

	rc, err := sse.New(key, base, 0)
	require.NoError(t, err)

	_, err = io.ReadAll(sse.NewReader(bytes.NewReader(truncated), rc, size))
	require.Error(t, err, "a truncated object was served as complete")
}

func TestWrongKeyRejected(t *testing.T) {
	const size = 5000

	c, _, base := newCipher(t)
	ct := encrypt(t, c, randomBytes(t, size))

	other, err := sse.NewKey()
	require.NoError(t, err)

	rc, err := sse.New(other, base, 0)
	require.NoError(t, err)

	_, err = io.ReadAll(sse.NewReader(bytes.NewReader(ct), rc, size))
	require.Error(t, err)
}

// TestPartIsolation covers multipart: parts share a data key, so a part read
// under the wrong part number must fail rather than return another part's
// bytes.
func TestPartIsolation(t *testing.T) {
	const size = sse.ChunkSize + 99

	key, err := sse.NewKey()
	require.NoError(t, err)

	base, err := sse.NewNonceBase()
	require.NoError(t, err)

	c1, err := sse.New(key, base, 1)
	require.NoError(t, err)

	plain := randomBytes(t, size)
	ct := encrypt(t, c1, plain)

	// Same key, same base, right part: readable.
	ok, err := sse.New(key, base, 1)
	require.NoError(t, err)

	got, err := io.ReadAll(sse.NewReader(bytes.NewReader(ct), ok, size))
	require.NoError(t, err)
	require.Equal(t, plain, got)

	// Same key, same base, wrong part: refused.
	wrong, err := sse.New(key, base, 2)
	require.NoError(t, err)

	_, err = io.ReadAll(sse.NewReader(bytes.NewReader(ct), wrong, size))
	require.Error(t, err)
}

// TestPartsDifferInCiphertext is the nonce-reuse guard: identical plaintext in
// two parts of one object must not produce identical ciphertext, because equal
// ciphertext would mean the keystream was reused.
func TestPartsDifferInCiphertext(t *testing.T) {
	key, err := sse.NewKey()
	require.NoError(t, err)

	base, err := sse.NewNonceBase()
	require.NoError(t, err)

	plain := bytes.Repeat([]byte("a"), 4096)

	c1, err := sse.New(key, base, 1)
	require.NoError(t, err)

	c2, err := sse.New(key, base, 2)
	require.NoError(t, err)

	require.NotEqual(t, encrypt(t, c1, plain), encrypt(t, c2, plain))
}

// TestChunksDifferInCiphertext is the same guard within one body: repeated
// plaintext across chunk boundaries must not repeat in the ciphertext.
func TestChunksDifferInCiphertext(t *testing.T) {
	c, _, _ := newCipher(t)

	plain := bytes.Repeat([]byte("b"), 3*sse.ChunkSize)
	ct := encrypt(t, c, plain)

	const stored = sse.ChunkSize + sse.TagSize

	require.NotEqual(t, ct[0:stored], ct[stored:2*stored])
	require.NotEqual(t, ct[stored:2*stored], ct[2*stored:3*stored])
}

func TestSizeMath(t *testing.T) {
	for _, size := range sizes {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			cipherSize := sse.CipherSize(size)

			plain, ok := sse.PlainSize(cipherSize)
			require.True(t, ok)
			require.Equal(t, size, plain)
		})
	}
}

// TestPlainSizeRejectsImpossible covers the scrubber's case: a file whose
// length no encryption of any body could have produced.
func TestPlainSizeRejectsImpossible(t *testing.T) {
	for _, bad := range []int64{1, sse.TagSize - 1, sse.TagSize, -1} {
		_, ok := sse.PlainSize(bad)
		require.False(t, ok, "cipher size %d was accepted", bad)
	}

	// A tag plus one byte is the shortest legal body.
	plain, ok := sse.PlainSize(sse.TagSize + 1)
	require.True(t, ok)
	require.Equal(t, int64(1), plain)
}

func TestWriteAfterClose(t *testing.T) {
	c, _, _ := newCipher(t)

	w := sse.NewWriter(io.Discard, c)
	require.NoError(t, w.Close())

	_, err := w.Write([]byte("x"))
	require.Error(t, err)

	require.NoError(t, w.Close(), "Close must be idempotent")
}

// TestEmptyBodyStoresNothing pins the zero-length case: no chunk, so no tag,
// so a zero-byte object stays zero bytes on disk.
func TestEmptyBodyStoresNothing(t *testing.T) {
	c, key, base := newCipher(t)

	var buf bytes.Buffer

	w := sse.NewWriter(&buf, c)
	require.NoError(t, w.Close())
	require.Zero(t, buf.Len())

	rc, err := sse.New(key, base, 0)
	require.NoError(t, err)

	got, err := io.ReadAll(sse.NewReader(bytes.NewReader(nil), rc, 0))
	require.NoError(t, err)
	require.Empty(t, got)
}

func TestNewRejectsBadSizes(t *testing.T) {
	key, err := sse.NewKey()
	require.NoError(t, err)

	base, err := sse.NewNonceBase()
	require.NoError(t, err)

	_, err = sse.New(key[:16], base, 0)
	require.Error(t, err, "a 128-bit key must be refused, not silently accepted")

	_, err = sse.New(key, base[:8], 0)
	require.Error(t, err)
}

// TestRandomRangeReads is the range-GET conformance property stated directly,
// over many random windows.
func TestRandomRangeReads(t *testing.T) {
	const size = 6*sse.ChunkSize + 1234

	c, key, base := newCipher(t)
	plain := randomBytes(t, size)
	ct := encrypt(t, c, plain)

	for range 200 {
		start := randInt64(t, size)
		length := randInt64(t, size-start+1)

		rc, err := sse.New(key, base, 0)
		require.NoError(t, err)

		r := sse.NewReader(bytes.NewReader(ct), rc, size)

		_, err = r.Seek(start, io.SeekStart)
		require.NoError(t, err)

		got := make([]byte, length)
		_, err = io.ReadFull(r, got)
		require.NoError(t, err)
		require.Equal(t, plain[start:start+length], got,
			"range [%d,%d) decrypted wrong", start, start+length)
	}
}

func randInt64(t *testing.T, n int64) int64 {
	t.Helper()

	if n <= 0 {
		return 0
	}

	v, err := rand.Int(rand.Reader, big.NewInt(n))
	require.NoError(t, err)

	return v.Int64()
}

func FuzzRoundTrip(f *testing.F) {
	f.Add([]byte(""))
	f.Add([]byte("a"))
	f.Add(bytes.Repeat([]byte("z"), sse.ChunkSize))
	f.Add(bytes.Repeat([]byte("q"), sse.ChunkSize+1))

	f.Fuzz(func(t *testing.T, plain []byte) {
		key, err := sse.NewKey()
		require.NoError(t, err)

		base, err := sse.NewNonceBase()
		require.NoError(t, err)

		wc, err := sse.New(key, base, 0)
		require.NoError(t, err)

		var buf bytes.Buffer

		w := sse.NewWriter(&buf, wc)
		_, err = w.Write(plain)
		require.NoError(t, err)
		require.NoError(t, w.Close())

		require.Equal(t, sse.CipherSize(int64(len(plain))), int64(buf.Len()))

		rc, err := sse.New(key, base, 0)
		require.NoError(t, err)

		got, err := io.ReadAll(sse.NewReader(bytes.NewReader(buf.Bytes()), rc, int64(len(plain))))
		require.NoError(t, err)
		require.Equal(t, len(plain), len(got))

		if len(plain) > 0 {
			require.Equal(t, plain, got)
		}
	})
}
