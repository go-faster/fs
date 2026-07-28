// Package reqid carries the S3 request ID — the value a client reads back from
// x-amz-request-id and from the RequestId of every S3 error body — through the
// request context and across the cluster hop.
//
// The ID exists to be diagnosed with. A failing client reports one thing about
// its request ("PutObject 500, RequestID: 55690797AEB458C9"), so that string
// has to be what finds the log line — not only on the node that answered, but
// on whichever peer actually failed underneath it. An ID that stops at the
// coordinator leaves the real cause unattributable: the peer logs (if it logs
// at all) with nothing tying its failure to the request that caused it.
package reqid

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// Header is the response header S3 clients read the ID from.
const Header = "x-amz-request-id"

// Field is the log field name the ID is recorded under, on every node it
// reaches. Grep for it.
const Field = "request_id"

type ctxKey struct{}

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

// MaxLen bounds an accepted request ID. We mint 16 characters; the slack is for
// a future format change surviving a rolling upgrade, not for callers to use.
const MaxLen = 64

// Valid reports whether id is safe to carry, log and put on the wire.
//
// This is not a formatting nicety. An ID arriving from another node is that
// node's bytes, and it ends up in a log field on every line the request
// produces: unbounded, it lets one request write megabytes into this node's
// log, and unrestricted, it puts terminal escapes into whatever reads that log.
// Bounding it also keeps the observability feature from breaking the data path
// — net/http refuses to send a header value containing control characters, so
// an ID we did not check could fail the fragment write itself.
//
// The charset is deliberately wider than the hex we generate: a peer running a
// newer binary that mints a different-shaped ID should still correlate, and an
// older peer that rejects one merely loses the correlation.
func Valid(id string) bool {
	if id == "" || len(id) > MaxLen {
		return false
	}

	for _, c := range []byte(id) {
		switch {
		case c >= '0' && c <= '9',
			c >= 'A' && c <= 'Z',
			c >= 'a' && c <= 'z',
			c == '-', c == '_':
		default:
			return false
		}
	}

	return true
}

// NewContext returns ctx carrying id.
func NewContext(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, ctxKey{}, id)
}

// FromContext returns the request ID carried by ctx, or "" when there is none —
// which is the normal case for work that no client request originated, such as
// scrub, repair and rebalance.
func FromContext(ctx context.Context) string {
	id, _ := ctx.Value(ctxKey{}).(string)

	return id
}
