package sigv4

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strconv"
	"strings"

	"github.com/go-faster/errors"
)

// chunkStringToSignAlgorithm is the string-to-sign prefix for aws-chunked
// per-chunk signatures.
const chunkStringToSignAlgorithm = "AWS4-HMAC-SHA256-PAYLOAD"

// maxChunkSize bounds a single aws-chunked chunk's declared payload length.
// The size is read from untrusted framing before the chunk signature is
// checked, so an unbounded value would let a client force a huge up-front
// allocation. 64 MiB is far above any real streaming chunk (AWS SDKs use
// 64 KiB–8 MiB) while capping the per-chunk buffer.
const maxChunkSize = 64 << 20

// ErrChunkSignature reports a streaming upload chunk whose signature does not
// chain correctly from the seed.
var ErrChunkSignature = errors.New("chunk signature mismatch")

// SignedStreaming reports whether the verified request used the signed
// aws-chunked variant (STREAMING-AWS4-HMAC-SHA256-PAYLOAD), whose chunks must
// be verified as the body is read. The unsigned-trailer variant has no
// per-chunk signatures, so its body needs no extra verification.
func (r *Result) SignedStreaming() bool {
	return r.signedChunks
}

// ChunkVerifyingReader wraps a signed aws-chunked body, decoding the framing
// and verifying each chunk's signature against the rolling chain seeded by the
// header signature. It yields the decoded payload bytes; a signature mismatch
// surfaces as a read error, so a tampered upload can never be stored. Only
// valid for a SignedStreaming result.
func (r *Result) ChunkVerifyingReader(body io.Reader) io.Reader {
	return &chunkVerifyingReader{
		src:        bufio.NewReader(body),
		signingKey: r.signingKey,
		scope:      r.scope,
		timestamp:  r.amzTime.UTC().Format(amzTimeFormat),
		prevSig:    r.seedSignature,
	}
}

type chunkVerifyingReader struct {
	src        *bufio.Reader
	signingKey []byte
	scope      string
	timestamp  string
	prevSig    string

	remaining int64 // bytes left in the current chunk's payload
	buf       []byte
	off       int
	done      bool
	err       error
}

func (c *chunkVerifyingReader) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}

	// Serve any buffered payload from the current chunk first.
	if c.off < len(c.buf) {
		n := copy(p, c.buf[c.off:])
		c.off += n

		return n, nil
	}

	if c.done {
		return 0, io.EOF
	}

	if err := c.nextChunk(); err != nil {
		c.err = err
		return 0, err
	}

	if c.done {
		return 0, io.EOF
	}

	n := copy(p, c.buf[c.off:])
	c.off += n

	return n, nil
}

// nextChunk reads and verifies the next chunk into c.buf.
func (c *chunkVerifyingReader) nextChunk() error {
	line, err := c.src.ReadString('\n')
	if err != nil {
		return errors.Wrap(err, "read chunk header")
	}

	header := strings.TrimRight(line, "\r\n")

	sizeStr, sig, ok := strings.Cut(header, ";")
	if !ok {
		return errors.Wrap(ErrChunkSignature, "chunk header missing signature")
	}

	size, err := strconv.ParseInt(sizeStr, 16, 64)
	if err != nil {
		return errors.Wrap(err, "parse chunk size")
	}

	// The size is attacker-controlled and reached before any signature check.
	// Reject a negative size (make([]byte, size) would panic) and one past the
	// per-chunk ceiling (a huge declared size would pre-allocate gigabytes and
	// exhaust memory). Real aws-chunked clients use KB-to-few-MB chunks, well
	// under the cap.
	if size < 0 || size > maxChunkSize {
		return errors.Wrapf(ErrChunkSignature, "chunk size %d out of range", size)
	}

	chunkSig := strings.TrimPrefix(sig, "chunk-signature=")

	// Read exactly size bytes of payload.
	data := make([]byte, size)
	if _, err := io.ReadFull(c.src, data); err != nil {
		return errors.Wrap(err, "read chunk data")
	}

	// Verify the chunk signature chains from the previous one.
	expected := c.signChunk(data)
	if !constantTimeEqual(expected, chunkSig) {
		return ErrChunkSignature
	}

	c.prevSig = expected

	// Consume the trailing CRLF after the payload.
	if _, err := c.src.ReadString('\n'); err != nil && !errors.Is(err, io.EOF) {
		return errors.Wrap(err, "read chunk terminator")
	}

	if size == 0 {
		// Final (zero-length) chunk: its signature is verified above; any
		// trailer that follows is not part of the payload.
		c.done = true

		return nil
	}

	c.buf = data
	c.off = 0

	return nil
}

// signChunk computes the expected signature for a chunk's data, chaining from
// c.prevSig.
func (c *chunkVerifyingReader) signChunk(data []byte) string {
	dataHash := sha256.Sum256(data)

	sts := strings.Join([]string{
		chunkStringToSignAlgorithm,
		c.timestamp,
		c.scope,
		c.prevSig,
		emptyPayloadHash,
		hex.EncodeToString(dataHash[:]),
	}, "\n")

	return hexHMAC(c.signingKey, sts)
}
