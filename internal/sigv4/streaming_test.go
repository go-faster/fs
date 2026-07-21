package sigv4

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// buildSignedChunks encodes payload into aws-chunked framing with per-chunk
// signatures chained from seed, using the same string-to-sign the reader
// verifies. chunkSize splits the payload; a final zero-length chunk closes it.
func buildSignedChunks(signingKey []byte, scope, timestamp, seed string, payload []byte, chunkSize int) string {
	var b strings.Builder

	prev := seed

	sign := func(data []byte) string {
		h := sha256.Sum256(data)
		sts := strings.Join([]string{
			chunkStringToSignAlgorithm, timestamp, scope, prev, emptyPayloadHash,
			hex.EncodeToString(h[:]),
		}, "\n")
		sig := hexHMAC(signingKey, sts)
		prev = sig

		return sig
	}

	for off := 0; off < len(payload); off += chunkSize {
		end := min(off+chunkSize, len(payload))
		data := payload[off:end]
		sig := sign(data)
		b.WriteString(strings.ToLower(strconv.FormatInt(int64(len(data)), 16)))
		b.WriteString(";chunk-signature=" + sig + "\r\n")
		b.Write(data)
		b.WriteString("\r\n")
	}

	// Final zero-length chunk.
	sig := sign(nil)
	b.WriteString("0;chunk-signature=" + sig + "\r\n\r\n")

	return b.String()
}

func newStreamingResult() *Result {
	cred := credential{accessKey: testAccessKey, date: "20130524", region: "us-east-1", service: "s3"}

	return &Result{
		AccessKey:     testAccessKey,
		Streaming:     true,
		signedChunks:  true,
		seedSignature: strings.Repeat("ab", 32),
		signingKey:    deriveSigningKey(testSecretKey, cred),
		scope:         cred.scope(),
		amzTime:       time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC),
	}
}

func TestChunkVerifyingReader_RoundTrip(t *testing.T) {
	res := newStreamingResult()
	require.True(t, res.SignedStreaming())

	payload := bytes.Repeat([]byte("go-faster"), 5000) // ~45 KiB across several chunks
	body := buildSignedChunks(res.signingKey, res.scope, res.amzTime.UTC().Format(amzTimeFormat),
		res.seedSignature, payload, 16*1024)

	got, err := io.ReadAll(res.ChunkVerifyingReader(strings.NewReader(body)))
	require.NoError(t, err)
	require.Equal(t, payload, got)
}

func TestChunkVerifyingReader_TamperedData(t *testing.T) {
	res := newStreamingResult()

	payload := []byte("the quick brown fox")
	body := buildSignedChunks(res.signingKey, res.scope, res.amzTime.UTC().Format(amzTimeFormat),
		res.seedSignature, payload, 1024)

	// Flip a payload byte after signing; the chunk signature no longer matches.
	tampered := []byte(body)
	idx := strings.Index(body, "fox")
	require.GreaterOrEqual(t, idx, 0)
	tampered[idx] = 'F'

	_, err := io.ReadAll(res.ChunkVerifyingReader(bytes.NewReader(tampered)))
	require.ErrorIs(t, err, ErrChunkSignature)
}

func TestChunkVerifyingReader_WrongSeed(t *testing.T) {
	res := newStreamingResult()

	payload := []byte("payload")
	// Sign the chunks against a different seed than the reader was given.
	body := buildSignedChunks(res.signingKey, res.scope, res.amzTime.UTC().Format(amzTimeFormat),
		strings.Repeat("cd", 32), payload, 1024)

	_, err := io.ReadAll(res.ChunkVerifyingReader(strings.NewReader(body)))
	require.ErrorIs(t, err, ErrChunkSignature)
}
