// Package transport is the peer replication transport for go-faster/fs cluster
// mode: the internal HTTP API nodes use to store, fetch and delete object
// fragments on each other (the payloads produced by internal/cluster/fragment).
//
// Security model (DESIGN.md Phase 7, "cluster-secret mutual auth,
// checksummed"): every request is HMAC-signed with the shared cluster secret
// (method, path, timestamp and sender bound into the signature; stale
// timestamps rejected), and every response carries an HMAC over the request
// signature plus the payload digest — so both sides prove knowledge of the
// secret, and a write ack from a peer that did not durably receive the exact
// bytes is impossible to forge. Payloads are SHA-256 checksummed end-to-end:
// the receiver hashes what it stores and the sender compares; reads stream the
// digest as an HTTP trailer that the client verifies at EOF.
//
// The transport is deliberately dumb: it moves named fragment payloads to and
// from a node-local Store, one disk namespace per request. Placement, quorum,
// repair and metadata all live above it (clusterstore).
package transport

import "github.com/go-faster/errors"

// Header and trailer names of the wire protocol.
const (
	// headerTimestamp carries the request's unix-seconds timestamp.
	headerTimestamp = "X-Cluster-Timestamp"
	// headerNode carries the sender's node ID.
	headerNode = "X-Cluster-Node"
	// headerAuth carries the request HMAC. A dedicated header (not
	// Authorization) so the cluster listener can also sit behind generic
	// reverse proxies that mangle Authorization.
	headerAuth = "X-Cluster-Auth"
	// headerRespAuth carries the response HMAC (mutual auth).
	headerRespAuth = "X-Cluster-Resp-Auth"
	// headerDigest carries the payload SHA-256 (response header on PUT, HTTP
	// trailer on GET).
	headerDigest = "X-Fragment-Sha256"
	// headerSize carries the fragment size on HEAD responses.
	headerSize = "X-Fragment-Size"
)

// maxClockSkew is how far a request timestamp may deviate from the receiver's
// clock before it is rejected (limits replay windows).
const maxClockSkew = 300 // seconds

// Sentinel errors surfaced by the client and server.
var (
	// ErrNotFound reports a fragment that does not exist on the peer.
	ErrNotFound = errors.New("fragment not found")
	// ErrUnauthorized reports a failed request or response authentication.
	ErrUnauthorized = errors.New("cluster auth failed")
	// ErrChecksumMismatch reports a payload whose digest did not match.
	ErrChecksumMismatch = errors.New("fragment checksum mismatch")
)
