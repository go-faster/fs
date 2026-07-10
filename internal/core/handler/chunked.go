package handler

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// awsChunkedReader decodes the aws-chunked transfer framing that AWS SDKs use
// for streaming uploads. Each chunk is framed as:
//
//	<hex-size>[;chunk-signature=<sig>]\r\n<size bytes>\r\n
//
// terminated by a zero-length chunk, optionally followed by trailing headers
// (the STREAMING-UNSIGNED-PAYLOAD-TRAILER variant). Chunk signatures and
// trailers are not verified here; the reader yields only the payload bytes.
type awsChunkedReader struct {
	reader    *bufio.Reader
	remaining int64
	done      bool
}

// newAWSChunkedReader creates a reader that decodes aws-chunked framing from r.
func newAWSChunkedReader(r io.Reader) *awsChunkedReader {
	return &awsChunkedReader{reader: bufio.NewReader(r)}
}

func (r *awsChunkedReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}

	// Advance to the next chunk header when the current chunk is exhausted.
	if r.remaining == 0 {
		if err := r.nextChunk(); err != nil {
			return 0, err
		}

		if r.done {
			return 0, io.EOF
		}
	}

	toRead := int64(len(p))
	if toRead > r.remaining {
		toRead = r.remaining
	}

	n, err := r.reader.Read(p[:toRead])
	r.remaining -= int64(n)

	// Consume the CRLF that terminates the chunk data once fully read.
	if r.remaining == 0 {
		_, _ = r.reader.ReadString('\n')
	}

	return n, err
}

// nextChunk reads and parses the next chunk-size header, setting remaining for
// a data chunk or marking the stream done at the terminating zero chunk.
func (r *awsChunkedReader) nextChunk() error {
	line, err := r.reader.ReadString('\n')
	if err != nil && line == "" {
		r.done = true

		return io.EOF
	}

	line = strings.TrimRight(line, "\r\n")

	// The size is the hex value before any ";chunk-signature=..." extension.
	sizeStr := line
	if idx := strings.IndexByte(line, ';'); idx != -1 {
		sizeStr = line[:idx]
	}

	size, parseErr := strconv.ParseInt(strings.TrimSpace(sizeStr), 16, 64)
	if parseErr != nil {
		// Not valid chunk framing; stop rather than corrupt the payload.
		r.done = true

		return io.EOF
	}

	if size == 0 {
		// Terminating chunk: discard any trailing headers and finish.
		r.drainTrailers()
		r.done = true

		return io.EOF
	}

	r.remaining = size

	return nil
}

// drainTrailers consumes any trailing headers after the terminating chunk up to
// the blank line that ends them.
func (r *awsChunkedReader) drainTrailers() {
	for {
		line, err := r.reader.ReadString('\n')
		if err != nil {
			return
		}

		if strings.TrimRight(line, "\r\n") == "" {
			return
		}
	}
}

// isAWSChunkedEncoding reports whether Content-Encoding indicates aws-chunked.
func isAWSChunkedEncoding(contentEncoding string) bool {
	return strings.Contains(contentEncoding, "aws-chunked")
}

// isAWSStreamingPayload reports whether x-amz-content-sha256 marks a streaming
// (chunked) payload.
func isAWSStreamingPayload(contentSha256 string) bool {
	return strings.HasPrefix(contentSha256, "STREAMING-")
}
