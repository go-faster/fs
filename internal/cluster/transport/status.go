package transport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/go-faster/errors"
)

// maxStatusBody bounds the status document a peer may return. The payload is a
// fixed set of counters; the limit stops an unverified peer response from
// consuming memory before its signature is checked.
const maxStatusBody = 1 << 20

// NodeStatus is a node's live runtime state: the state that exists only inside
// the running process — queue depths, runner progress, scrub totals — as
// opposed to the passive state (topology, capacity, schema version, rebalance
// election) any process can read from etcd. Fields are additive; an older peer
// simply leaves the ones it does not know zero.
type NodeStatus struct {
	// NodeID is the reporting node's registry identity.
	NodeID string `json:"node_id"`
	// Version is the node's binary version, for spotting a half-upgraded
	// cluster.
	Version string `json:"version,omitempty"`
	// SchemaVersion is the schema version the node's binary implements.
	SchemaVersion int `json:"schema_version,omitempty"`
	// UptimeSeconds is how long the node process has been running.
	UptimeSeconds float64 `json:"uptime_seconds,omitempty"`
	// RepairQueueDepth is the node's pending async remainder/repair backlog.
	RepairQueueDepth int `json:"repair_queue_depth"`
	// Rebalance is the node's rebalance runner.
	Rebalance NodeRebalance `json:"rebalance"`
	// Scrub is the node's cumulative scrub and repair totals.
	Scrub NodeScrub `json:"scrub"`
	// Disks is what each of the node's disks currently holds. Only the node
	// itself can answer this: the control plane records capacity, and capacity
	// cannot tell an emptied disk from a lightly used one.
	Disks []NodeDisk `json:"disks,omitempty"`
	// Plane is which partitioning this node is actually routing by.
	Plane NodePlane `json:"plane,omitempty"`
}

// NodePlane is the sharded metadata plane as one node sees it.
//
// Only the node can answer this, and it is the one plane question the control
// plane cannot. Routing is lazy by design — a node refreshes its map when a
// peer tells it that it is behind, and not otherwise — so a node that takes no
// traffic for a range that moved can sit on a stale revision indefinitely with
// nothing to notice. etcd holds the map; this is what each node believes about
// it, and the disagreement is the thing worth seeing.
type NodePlane struct {
	// Enabled reports whether this node runs the sharded plane at all.
	Enabled bool `json:"enabled,omitempty"`
	// Revision is the map revision this node is routing by. Zero means it has
	// not loaded one.
	Revision int64 `json:"revision,omitempty"`
	// Owned is how many ranges the node serves, Replicated how many it holds
	// for someone else as a follower or a learner.
	Owned      int `json:"owned,omitempty"`
	Replicated int `json:"replicated,omitempty"`
}

// NodeDisk is what one of the node's disks holds right now. It is the drain
// signal: an orchestrator decommissioning a node watches HasData go false
// before it deletes the volume.
type NodeDisk struct {
	// ID is the disk's registry identity, matching the topology.
	ID string `json:"id"`
	// HasData reports whether the disk still holds any fragment. False means
	// drained — exactly, not approximately, which is why this is a boolean and
	// not a byte count (see diskstore.Store.HasData).
	HasData bool `json:"has_data"`
	// Err is why the disk could not be probed, if it could not. A disk that
	// failed to answer must never read as drained, so HasData stays false and
	// this says why.
	Err string `json:"error,omitempty"`

	// Fragments and Bytes are what the node's occupancy index says the disk
	// holds. They turn a drain from a yes/no into a progress readout — "still
	// 4.2 GiB to move" — which is the difference between waiting and knowing
	// how long. Meaningful only when Counted.
	Fragments int64 `json:"fragments,omitempty"`
	Bytes     int64 `json:"bytes,omitempty"`
	// Counted reports that the index has been anchored by a scan. Until it is,
	// Fragments and Bytes are zero and mean nothing — a node that restarted
	// uncleanly walks its disks before it can count them. HasData is the
	// authority on emptiness either way; these two are the progress bar, and a
	// progress bar is not something to delete a volume over.
	Counted bool `json:"counted,omitempty"`
}

// NodeRebalance is the node-local rebalance runner's state and progress. State
// mirrors the admin API's runner states ("idle", "waiting", "running",
// "paused", "done", "failed"); it is empty on a node with no runner.
type NodeRebalance struct {
	State     string `json:"state,omitempty"`
	Objects   int    `json:"objects"`
	Relocated int    `json:"relocated"`
	Failed    int    `json:"failed"`
	Err       string `json:"error,omitempty"`
}

// NodeScrub is the node's cumulative scrub totals since process start.
type NodeScrub struct {
	Passes           int64 `json:"passes"`
	Objects          int64 `json:"objects"`
	Repaired         int64 `json:"repaired"`
	Failed           int64 `json:"failed"`
	RebuiltFragments int64 `json:"rebuilt_fragments"`
	SweptStale       int64 `json:"swept_stale"`
	CorruptReplicas  int64 `json:"corrupt_replicas"`
	Converted        int64 `json:"converted"`
	// ECUnverified reports that the last pass saw an EC set failing parity
	// verification.
	ECUnverified bool `json:"ec_unverified"`
	// OldestVerified is the least recent verification among the objects this
	// node holds, and NeverVerified how many have never been checked.
	//
	// Together they are the coverage the totals above cannot give: counts of
	// work done say how busy a scrubber was, not whether it ever reached the
	// objects at the back of a disk. A cycle failing to keep up shows as an
	// oldest that keeps receding, or a never that does not fall.
	OldestVerified time.Time `json:"oldest_verified,omitzero"`
	NeverVerified  int64     `json:"never_verified,omitempty"`
	// Held is how many objects this node holds, the denominator for the two
	// above. Objects, higher up, counts what its passes have swept, which is a
	// different number and grows without bound.
	Held int64 `json:"held,omitempty"`
}

// StatusFunc reports the node's live state. It runs on a peer's request, so it
// must be cheap and must not block on the control plane.
type StatusFunc func(ctx context.Context) (NodeStatus, error)

// serveStatus returns the node's live state as a signed JSON document. The
// response signature covers the body digest, so a tampered status document
// cannot be passed off as this node's.
func (s *Server) serveStatus(w http.ResponseWriter, r *http.Request) {
	if s.nodeStatus == nil {
		http.Error(w, "node status not served here", http.StatusNotImplemented)
		return
	}

	st, err := s.nodeStatus(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	body, err := json.Marshal(st)
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

// Status fetches the peer's live runtime state. A peer that does not serve it —
// an older binary (no route) or one wired without a status source — reports
// ErrUnsupported, so a rolling upgrade degrades to "not reporting" rather than
// to an error.
func (c *Client) Status(ctx context.Context) (NodeStatus, error) {
	u := *c.base
	u.Path = strings.TrimSuffix(c.base.Path, "/") + "/v1/status"
	u.RawPath = ""

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return NodeStatus{}, errors.Wrap(err, "build request")
	}

	reqSig := c.secret.authenticate(req, c.node, c.now())

	resp, err := c.send(req)
	if err != nil {
		return NodeStatus{}, errors.Wrap(err, "get node status")
	}

	defer drainClose(resp)

	if err := statusError(resp); err != nil {
		// An old peer has no /v1/status route and answers 404; treat it the same
		// as an explicit 501.
		if errors.Is(err, ErrNotFound) {
			return NodeStatus{}, ErrUnsupported
		}

		return NodeStatus{}, err
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxStatusBody))
	if err != nil {
		return NodeStatus{}, errors.Wrap(err, "read node status")
	}

	digest := resp.Header.Get(headerDigest)
	if err := c.secret.verifyResponse(reqSig, digest, resp.Header.Get(headerRespAuth)); err != nil {
		return NodeStatus{}, err
	}

	sum := sha256.Sum256(body)
	if digest != hex.EncodeToString(sum[:]) {
		return NodeStatus{}, errors.Wrap(ErrChecksumMismatch, "node status digest mismatch")
	}

	var st NodeStatus
	if err := json.Unmarshal(body, &st); err != nil {
		return NodeStatus{}, errors.Wrap(err, "decode node status")
	}

	return st, nil
}
