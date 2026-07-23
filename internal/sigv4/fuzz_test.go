package sigv4

import (
	"bufio"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newChunkReader builds a chunk-decoding reader over raw framing for tests.
func newChunkReader(framing string) *chunkVerifyingReader {
	return &chunkVerifyingReader{
		src:        bufio.NewReader(strings.NewReader(framing)),
		signingKey: make([]byte, 32),
		scope:      "20200101/us-east-1/s3/aws4_request",
		timestamp:  "20200101T000000Z",
		prevSig:    strings.Repeat("0", 64),
	}
}

// TestChunkSizeBounds is the regression test for the fuzz-found DoS: a
// negative or oversized declared chunk length must be rejected with an error,
// never reach make([]byte, size) (which panics on a negative length and
// pre-allocates gigabytes on a huge one).
func TestChunkSizeBounds(t *testing.T) {
	for name, header := range map[string]string{
		"negative": "-1;chunk-signature=x\r\n",
		"maxint64": "7fffffffffffffff;chunk-signature=x\r\n",
		"over_cap": "5000000;chunk-signature=x\r\n", // 80 MiB > 64 MiB cap.
	} {
		t.Run(name, func(t *testing.T) {
			c := newChunkReader(header + strings.Repeat("A", 4096))

			_, err := io.Copy(io.Discard, c)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrChunkSignature)
		})
	}
}

// FuzzParseAuthorization exercises the Authorization-header parser with
// arbitrary input: it must return a value or an error, never panic.
func FuzzParseAuthorization(f *testing.F) {
	f.Add("AWS4-HMAC-SHA256 Credential=AKID/20200101/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc")
	f.Add("AWS4-HMAC-SHA256 Credential=,SignedHeaders=,Signature=")
	f.Add("")
	f.Add("AWS4-HMAC-SHA256 ")
	f.Add("Credential=a/b/c/d/e, SignedHeaders=host, Signature=deadbeef")

	f.Fuzz(func(t *testing.T, header string) {
		_, _, _, _ = parseAuthorization(header)
	})
}

// FuzzParseCredential exercises the credential-scope parser.
func FuzzParseCredential(f *testing.F) {
	f.Add("AKID/20200101/us-east-1/s3/aws4_request")
	f.Add("////")
	f.Add("")
	f.Add("a/b/c/d/e/f/g")

	f.Fuzz(func(t *testing.T, s string) {
		_, _ = parseCredential(s)
	})
}

// FuzzChunkReader feeds arbitrary aws-chunked framing to the chunk-decoding
// reader. The reader parses attacker-controlled chunk sizes and payloads
// before any signature check; decoding must fail gracefully (an error), never
// panic or attempt an unbounded allocation.
func FuzzChunkReader(f *testing.F) {
	f.Add([]byte("5;chunk-signature=" + strings.Repeat("0", 64) + "\r\nhello\r\n0;chunk-signature=" + strings.Repeat("0", 64) + "\r\n\r\n"))
	f.Add([]byte("0;chunk-signature=\r\n"))
	f.Add([]byte("-1;chunk-signature=x\r\n"))
	f.Add([]byte("ffffffffffffffff;chunk-signature=x\r\n"))
	f.Add([]byte(""))

	f.Fuzz(func(t *testing.T, framing []byte) {
		c := &chunkVerifyingReader{
			src:        bufio.NewReader(strings.NewReader(string(framing))),
			signingKey: make([]byte, 32),
			scope:      "20200101/us-east-1/s3/aws4_request",
			timestamp:  "20200101T000000Z",
			prevSig:    strings.Repeat("0", 64),
		}

		// Drain until error/EOF, bounding total output so a valid-looking
		// stream can't run the fuzzer out of memory legitimately.
		_, _ = io.Copy(io.Discard, io.LimitReader(c, 1<<20))
	})
}
