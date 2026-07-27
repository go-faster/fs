package sse_test

import (
	"bytes"
	"io"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/sse"
)

// TestPipeRoundTrip is the cluster path end to end: pull plaintext through the
// encrypting reader (as the fragmenter does), then back through the decrypting
// reader (as a read does).
func TestPipeRoundTrip(t *testing.T) {
	for _, size := range sizes {
		t.Run(strconv.FormatInt(size, 10), func(t *testing.T) {
			c, key, base := newCipher(t)
			plain := randomBytes(t, size)

			ct, err := io.ReadAll(sse.NewEncryptingReader(bytes.NewReader(plain), c))
			require.NoError(t, err)
			require.Equal(t, sse.CipherSize(size), int64(len(ct)),
				"the pull side must produce exactly what the push side would")

			rc, err := sse.New(key, base, 0)
			require.NoError(t, err)

			got, err := io.ReadAll(sse.NewDecryptingReader(bytes.NewReader(ct), rc, size))
			require.NoError(t, err)
			require.Equal(t, plain, got)
		})
	}
}

// TestPipeMatchesWriter pins the two encryption paths to the same bytes: an
// object written through storagefs and one written through the cluster must be
// byte-identical, or the format has quietly forked.
func TestPipeMatchesWriter(t *testing.T) {
	key, err := sse.NewKey()
	require.NoError(t, err)

	base, err := sse.NewNonceBase()
	require.NoError(t, err)

	plain := randomBytes(t, 3*sse.ChunkSize+991)

	wc, err := sse.New(key, base, 0)
	require.NoError(t, err)

	var pushed bytes.Buffer

	w := sse.NewWriter(&pushed, wc)
	_, err = w.Write(plain)
	require.NoError(t, err)
	require.NoError(t, w.Close())

	rc, err := sse.New(key, base, 0)
	require.NoError(t, err)

	pulled, err := io.ReadAll(sse.NewEncryptingReader(bytes.NewReader(plain), rc))
	require.NoError(t, err)

	require.Equal(t, pushed.Bytes(), pulled)
}

// TestPipeReadInArbitraryPieces: the chunk boundaries must not depend on how
// the consumer sizes its reads.
func TestPipeReadInArbitraryPieces(t *testing.T) {
	const size = 2*sse.ChunkSize + 333

	c, key, base := newCipher(t)
	plain := randomBytes(t, size)

	full, err := io.ReadAll(sse.NewEncryptingReader(bytes.NewReader(plain), c))
	require.NoError(t, err)

	for _, step := range []int{1, 7, 4096, sse.ChunkSize} {
		t.Run(strconv.Itoa(step), func(t *testing.T) {
			ec, err := sse.New(key, base, 0)
			require.NoError(t, err)

			r := sse.NewEncryptingReader(bytes.NewReader(plain), ec)

			var got []byte

			buf := make([]byte, step)

			for {
				n, err := r.Read(buf)
				got = append(got, buf[:n]...)

				if err == io.EOF {
					break
				}

				require.NoError(t, err)
			}

			require.Equal(t, full, got)
		})
	}
}

// TestDecryptingReaderRejectsTampering: the cluster read path must refuse
// damaged bytes too, not just the file-backed one.
func TestDecryptingReaderRejectsTampering(t *testing.T) {
	const size = 2*sse.ChunkSize + 100

	c, key, base := newCipher(t)
	plain := randomBytes(t, size)

	ct, err := io.ReadAll(sse.NewEncryptingReader(bytes.NewReader(plain), c))
	require.NoError(t, err)

	damaged := bytes.Clone(ct)
	damaged[len(damaged)/2] ^= 0x01

	rc, err := sse.New(key, base, 0)
	require.NoError(t, err)

	_, err = io.ReadAll(sse.NewDecryptingReader(bytes.NewReader(damaged), rc, size))
	require.Error(t, err)
}

// TestDecryptingReaderRejectsTruncation: a short stream must fail rather than
// return a short object.
func TestDecryptingReaderRejectsTruncation(t *testing.T) {
	const size = 3 * sse.ChunkSize

	c, key, base := newCipher(t)

	ct, err := io.ReadAll(sse.NewEncryptingReader(bytes.NewReader(randomBytes(t, size)), c))
	require.NoError(t, err)

	rc, err := sse.New(key, base, 0)
	require.NoError(t, err)

	short := ct[:len(ct)-(sse.ChunkSize+sse.TagSize)]

	_, err = io.ReadAll(sse.NewDecryptingReader(bytes.NewReader(short), rc, size))
	require.Error(t, err)
}
