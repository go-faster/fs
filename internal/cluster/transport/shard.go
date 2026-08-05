package transport

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster/metastore"
	"github.com/go-faster/fs/internal/cluster/rangemap"
)

// maxShardBody bounds a shard response. Larger than an index page because a
// scan carries whole entries rather than listing projections, and still the
// reader's own bound rather than a promise the peer made.
const maxShardBody = 16 << 20

// maxShardLimit caps how many entries one scan may ask for, so a peer cannot be
// made to assemble an unbounded page.
const maxShardLimit = 10_000

// Shard operations. One endpoint carries all of them rather than a route each:
// unlike the fragment routes, which are REST-shaped resources with distinct
// verbs, this is one cohesive RPC surface whose members differ only in their
// arguments. Nine near-identical handlers would be nine places to get the
// signing, the bounds and the error mapping subtly different.
const (
	ShardOpPut      = "put"
	ShardOpGet      = "get"
	ShardOpDelete   = "delete"
	ShardOpUsage    = "usage"
	ShardOpBuckets  = "buckets"
	ShardOpScan     = "scan"
	ShardOpVerified = "verified"
	ShardOpCoverage = "coverage"
	ShardOpReset    = "reset"
	// ShardOpApply replays an owner's committed batch on a follower.
	ShardOpApply = "apply"
	// ShardOpMeasure reports a range's size and where it would divide.
	//
	// Asked of the owner, because the owner is the only node that holds the
	// range — a controller deciding splits from its own shard would be
	// measuring whichever ranges it happens to own and calling that the
	// cluster.
	ShardOpMeasure = "measure"
	// ShardOpBackfill reads one bounded step of a range out of its owner, for a
	// learner being copied into.
	//
	// A read rather than a write, because the learner pulls: it is the node that
	// knows how far it has got, and a cursor kept where the data lands cannot
	// disagree with the data. The write half needs no operation at all — the
	// learner stores what it pulled into its own shard.
	ShardOpBackfill = "backfill"
	// ShardOpCaughtUp asks a learner whether the copy of a range has finished.
	//
	// Asked of the learner, because the learner is the only node that knows.
	// The owner knows what it sent and the controller knows what it decided;
	// neither knows what landed, and a promotion decided on either would
	// promote a node holding part of a range.
	ShardOpCaughtUp = "caught_up"
)

// ShardRequest is one operation against a peer's shard.
type ShardRequest struct {
	Op     string `json:"op"`
	Bucket string `json:"bucket,omitempty"`
	Key    string `json:"key,omitempty"`
	Prefix string `json:"prefix,omitempty"`
	After  string `json:"after,omitempty"`
	Limit  int    `json:"limit,omitempty"`
	// Range names which range a scan is confined to. The peer refuses a range
	// it does not serve, so a stale map cannot quietly produce a page with a
	// hole in it.
	Range *rangemap.Range `json:"range,omitempty"`
	// Entry is the record a put carries.
	Entry *metastore.Entry `json:"entry,omitempty"`
	// Records are the verification stamps.
	Records []metastore.Verification `json:"records,omitempty"`
	// Batch is an owner's committed batch, as pebble recorded it, for a
	// follower to replay. Opaque here on purpose: the wire carries what the
	// owner applied rather than a re-description of it, so a follower's state
	// is the owner's state and not a reconstruction that could differ.
	Batch []byte `json:"batch,omitempty"`
	// Cursor is where a backfill step resumes: the key the last step stopped
	// before. Separate from After, which is a scan's position inside a bucket —
	// this one is a position in the whole key space, and conflating them would
	// have a resumed backfill bounded by a bucket it was never confined to.
	Cursor string `json:"cursor,omitempty"`
}

// ShardResponse is the peer's answer.
type ShardResponse struct {
	// NotFollowed reports that the peer does not replicate the named range.
	//
	// Separate from NotOwned because they are different mistakes: NotOwned
	// means the caller's map is stale, NotFollowed means the *sender's*
	// follower set is, and the two are fixed by different parties.
	NotFollowed bool `json:"not_followed,omitempty"`

	// NotOwned reports that the key or range is outside what the peer serves.
	//
	// Carried as a field rather than an HTTP status because it is an answer,
	// not a failure: the caller refetches the map and retries, and folding it
	// into a 4xx would make it indistinguishable from a request the peer could
	// not parse.
	NotOwned bool `json:"not_owned,omitempty"`

	Found    bool                `json:"found,omitempty"`
	Entry    *metastore.Entry    `json:"entry,omitempty"`
	Entries  []metastore.Entry   `json:"entries,omitempty"`
	Buckets  []string            `json:"buckets,omitempty"`
	Usage    *metastore.Usage    `json:"usage,omitempty"`
	Coverage *metastore.Coverage `json:"coverage,omitempty"`

	// Bytes is a range's estimated size, for ShardOpMeasure.
	Bytes uint64 `json:"bytes,omitempty"`
	// SplitAt is where the range would divide, empty when the owner found no
	// point — a range with nothing in it, or one whose boundary is already
	// deeper than the search can reach.
	SplitAt string `json:"split_at,omitempty"`

	// Cursor is where the next backfill step resumes, empty when the range has
	// been walked to its end.
	Cursor string `json:"cursor,omitempty"`
	// CaughtUp reports that the peer has finished being copied into for the
	// range, for ShardOpCaughtUp.
	CaughtUp bool `json:"caught_up,omitempty"`
	// Done reports that a backfill step reached the end of the range.
	//
	// Carried rather than inferred from an empty Cursor, because a step that
	// stopped exactly on the last key would report both — and a learner that
	// read "no cursor" as "start over" would copy the range forever.
	Done bool `json:"done,omitempty"`
}

// ErrUnknownShardOp reports an operation the peer does not implement.
//
// A ShardFunc returns it, and the server answers 501 — so a newer caller asking
// an older peer for something it has never heard of degrades exactly as a peer
// without a shard at all does, rather than reading as a server fault.
var ErrUnknownShardOp = errors.New("unknown shard operation")

// ShardFunc answers a shard request from the node's own shard.
type ShardFunc func(ctx context.Context, req ShardRequest) (ShardResponse, error)

// WithShard makes the node serve its metadata shard to peers
// (POST /v1/shard) — the wire under a remote Backend.
func WithShard(fn ShardFunc) ServerOption {
	return func(s *Server) { s.shard = fn }
}

// serveShard answers a shard request as a signed JSON document, under the same
// contract as every other peer response: the signature covers the body digest,
// so a tampered answer cannot be passed off as this node's.
func (s *Server) serveShard(w http.ResponseWriter, r *http.Request) {
	if s.shard == nil {
		http.Error(w, "metadata shard not served here", http.StatusNotImplemented)

		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxShardBody))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)

		return
	}

	var req ShardRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		http.Error(w, "invalid shard request", http.StatusBadRequest)

		return
	}

	if req.Limit > maxShardLimit {
		req.Limit = maxShardLimit
	}

	out, err := s.shard(r.Context(), req)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, ErrUnknownShardOp) {
			status = http.StatusNotImplemented
		}

		http.Error(w, err.Error(), status)

		return
	}

	body, err := json.Marshal(out)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])

	w.Header().Set(headerDigest, digest)
	w.Header().Set(headerRespAuth, s.secret.signResponse(r.Header.Get(headerAuth), digest))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	_, _ = w.Write(body)
}

// Shard runs one operation against a peer's metadata shard.
//
// A peer that does not serve one — an older binary, or a node wired without a
// shard — reports ErrUnsupported, so a partially upgraded cluster degrades
// rather than erroring.
func (c *Client) Shard(ctx context.Context, req ShardRequest) (ShardResponse, error) {
	u := *c.base
	u.Path = strings.TrimSuffix(c.base.Path, "/") + "/v1/shard"
	u.RawPath = ""
	u.RawQuery = ""

	payload, err := json.Marshal(req)
	if err != nil {
		return ShardResponse{}, errors.Wrap(err, "encode shard request")
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(payload))
	if err != nil {
		return ShardResponse{}, errors.Wrap(err, "build request")
	}

	httpReq.Header.Set("Content-Type", "application/json")

	reqSig := c.secret.authenticate(httpReq, c.node, c.now())

	resp, err := c.send(httpReq)
	if err != nil {
		return ShardResponse{}, errors.Wrap(err, "shard request")
	}

	defer drainClose(resp)

	if err := statusError(resp); err != nil {
		if errors.Is(err, ErrNotFound) {
			return ShardResponse{}, ErrUnsupported
		}

		return ShardResponse{}, err
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxShardBody))
	if err != nil {
		return ShardResponse{}, errors.Wrap(err, "read shard response")
	}

	digest := resp.Header.Get(headerDigest)
	if err := c.secret.verifyResponse(reqSig, digest, resp.Header.Get(headerRespAuth)); err != nil {
		return ShardResponse{}, err
	}

	sum := sha256.Sum256(body)
	if digest != hex.EncodeToString(sum[:]) {
		return ShardResponse{}, errors.Wrap(ErrChecksumMismatch, "shard response digest mismatch")
	}

	var out ShardResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return ShardResponse{}, errors.Wrap(err, "decode shard response")
	}

	return out, nil
}
