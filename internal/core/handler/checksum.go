package handler

import (
	"net/http"
	"strings"

	"github.com/go-faster/fs/internal/checksum"
)

// Request headers for the client-visible checksum family.
const (
	// checksumAlgorithmHeader names the algorithm to compute.
	checksumAlgorithmHeader = "x-amz-checksum-algorithm"
	// sdkChecksumAlgorithmHeader is what the AWS SDKs send when the caller
	// asked for a checksum but supplied no digest of their own.
	sdkChecksumAlgorithmHeader = "x-amz-sdk-checksum-algorithm"
	// checksumModeHeader turns checksum reporting on for a read.
	checksumModeHeader = "x-amz-checksum-mode"
	// checksumTypeHeader distinguishes COMPOSITE from FULL_OBJECT.
	checksumTypeHeader = "x-amz-checksum-type"
)

// requestChecksum reads the algorithm and digest a write asks for.
//
// The algorithm can arrive three ways and they are tried most-explicit first:
// the header naming it, the SDK's own header, and — because a client may send
// only the digest — the presence of a per-algorithm digest header.
func requestChecksum(r *http.Request) (algorithm, digest string) {
	algorithm = strings.TrimSpace(r.Header.Get(checksumAlgorithmHeader))
	if algorithm == "" {
		algorithm = strings.TrimSpace(r.Header.Get(sdkChecksumAlgorithmHeader))
	}

	if algorithm != "" {
		if a, err := checksum.Parse(algorithm); err == nil && a != "" {
			return string(a), r.Header.Get(a.Header())
		}

		// Unknown algorithm: pass it through so the storage layer refuses it
		// rather than the request quietly losing the checksum it asked for.
		return algorithm, ""
	}

	for _, a := range []checksum.Algorithm{
		checksum.CRC32, checksum.CRC32C, checksum.CRC64NVME, checksum.SHA1, checksum.SHA256,
	} {
		if v := r.Header.Get(a.Header()); v != "" {
			return string(a), v
		}
	}

	return "", ""
}

// checksumRequested reports whether a read asked for the checksum.
//
// S3 omits it by default and returns it only under this header, which is what
// lets a client tell "this object has no checksum" from "I did not ask". Both
// are answered by the header's absence otherwise, so the distinction has to
// live in the request.
func checksumRequested(r *http.Request) bool {
	return strings.EqualFold(strings.TrimSpace(r.Header.Get(checksumModeHeader)), "ENABLED")
}

// writeChecksum reports an object's checksum on a read, when the request asked.
func writeChecksum(w http.ResponseWriter, r *http.Request, algorithm, digest, kind string) {
	if !checksumRequested(r) || algorithm == "" || digest == "" {
		return
	}

	a, err := checksum.Parse(algorithm)
	if err != nil || a == "" {
		return
	}

	w.Header().Set(a.Header(), digest)

	if kind != "" {
		w.Header().Set(checksumTypeHeader, kind)
	}
}
