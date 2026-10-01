// Package reqid mints the S3 request ID — the value a client reads back from
// x-amz-request-id and from the RequestId of every S3 error body — so a failing
// client's report finds the matching log line.
package reqid

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// Header is the response header S3 clients read the ID from.
const Header = "x-amz-request-id"

// Field is the log field name the ID is recorded under. Grep for it.
const Field = "request_id"

// New returns a random 16-hex-character request identifier, uppercased to match
// what AWS emits.
func New() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// A request that could not be given an ID is still worth serving: the
		// all-zero ID marks the log line as un-correlatable instead of failing
		// the request over a diagnostic.
		return "0000000000000000"
	}

	return strings.ToUpper(hex.EncodeToString(b[:]))
}
