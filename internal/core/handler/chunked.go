package handler

import (
	"bufio"
	"io"
	"strconv"
	"strings"
)

// awsChunkedReader decodes AWS Signature V4 chunked content encoding
type awsChunkedReader struct {
	reader    *bufio.Reader
	remaining int64
	done      bool
}

// newAWSChunkedReader creates a reader that decodes AWS chunked encoding
func newAWSChunkedReader(r io.Reader) *awsChunkedReader {
	return &awsChunkedReader{
		reader: bufio.NewReader(r),
	}
}

func (r *awsChunkedReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}

	// If we have remaining data from current chunk, read it
	if r.remaining > 0 {
		toRead := int64(len(p))
		if toRead > r.remaining {
			toRead = r.remaining
		}

		n, err := r.reader.Read(p[:toRead])
		r.remaining -= int64(n)

		// After reading a chunk, we need to consume the trailing \r\n
		if r.remaining == 0 {
			// Read the trailing CRLF
			_, _ = r.reader.ReadString('\n')
		}

		return n, err
	}

	// Read the next chunk header
	// Format: <hex-size>;chunk-signature=<signature>\r\n
	line, err := r.reader.ReadString('\n')
	if err != nil {
		if err == io.EOF && line == "" {
			r.done = true
			return 0, io.EOF
		}

		return 0, err
	}

	line = strings.TrimSuffix(line, "\r\n")
	line = strings.TrimSuffix(line, "\n")

	// Parse chunk size (hex before semicolon or end of line)
	sizeStr := line
	if idx := strings.Index(line, ";"); idx != -1 {
		sizeStr = line[:idx]
	}

	size, err := strconv.ParseInt(sizeStr, 16, 64)
	if err != nil {
		// Not chunked encoding, this might be raw data
		// Return what we have and let caller handle it
		r.done = true
		return 0, io.EOF
	}

	if size == 0 {
		// Final chunk - read the trailing empty line
		_, _ = r.reader.ReadString('\n')
		r.done = true

		return 0, io.EOF
	}

	r.remaining = size

	return r.Read(p)
}

// isAWSChunkedEncoding checks if the request uses AWS chunked encoding
func isAWSChunkedEncoding(contentEncoding string) bool {
	return strings.Contains(contentEncoding, "aws-chunked")
}

// isAWSStreamingPayload checks if the request uses AWS streaming payload signature
func isAWSStreamingPayload(contentSha256 string) bool {
	return strings.HasPrefix(contentSha256, "STREAMING-")
}
