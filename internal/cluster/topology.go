// Package cluster holds the shared domain types for go-faster/fs cluster mode:
// the failure-domain topology (rack → server → disk) that the etcd control
// plane publishes and the placement function consumes. It has no external
// dependencies and no I/O — the etcd integration and the replication transport
// live in sibling packages.
package cluster

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"
)

// NodeID uniquely identifies a cluster member.
type NodeID string

// DiskID uniquely identifies a disk. It only needs to be unique within its
// node; placement treats (NodeID, DiskID) as the physical target.
type DiskID string

// Disk is one storage device on a node.
type Disk struct {
	// ID identifies the disk within its node.
	ID DiskID
	// Weight is the relative capacity weight used by weighted placement. A disk
	// with weight <= 0 is drained: it is never chosen for new placement (used
	// when decommissioning or when a disk is full/faulted).
	Weight float64
}

// Node is a cluster member and the disks it exposes. Rack is its coarsest
// failure domain; placement spreads copies across distinct racks first, then
// distinct nodes, then distinct disks.
type Node struct {
	// ID identifies the node.
	ID NodeID
	// Addr is the node's cluster-listener address (host:port). Placement does
	// not use it; it is carried for the caller that dispatches to targets.
	Addr string
	// Rack is the failure-domain label. Nodes sharing a rack share fate (power,
	// switch); an empty rack means "unknown", treated as its own single domain
	// keyed by node so such nodes never falsely appear rack-diverse.
	Rack string
	// Disks are the node's storage devices.
	Disks []Disk
}

// Topology is an immutable snapshot of cluster membership at a given placement
// epoch. It is produced by the control plane (etcd revision → epoch) and is a
// pure input to placement; callers must not mutate a Topology after publishing
// it.
type Topology struct {
	// Epoch is the monotonically increasing version of this snapshot (the etcd
	// revision it was derived from). Placement output is stable within an epoch.
	Epoch uint64
	// Nodes are the members. Order is not significant to placement.
	Nodes []Node
}

// DiskCount reports the number of disks across all nodes, including drained
// ones.
func (t *Topology) DiskCount() int {
	n := 0
	for i := range t.Nodes {
		n += len(t.Nodes[i].Disks)
	}

	return n
}

// Signature is a stable digest of everything placement depends on: node IDs,
// racks, disk IDs and weights — sorted, so registration order does not matter.
// Addresses and the epoch are excluded: a node re-registering at a new address
// (or any registry churn that bumps the epoch without changing membership)
// moves no data and must not read as a placement change. Equal signatures ⇒
// identical placement for every object.
func (t *Topology) Signature() string {
	lines := make([]string, 0, len(t.Nodes))

	for i := range t.Nodes {
		n := &t.Nodes[i]

		disks := make([]string, 0, len(n.Disks))
		for _, d := range n.Disks {
			disks = append(disks, string(d.ID)+"="+strconv.FormatFloat(d.Weight, 'g', -1, 64))
		}

		sort.Strings(disks)

		lines = append(lines, string(n.ID)+"\x00"+n.Rack+"\x00"+strings.Join(disks, ","))
	}

	sort.Strings(lines)

	sum := sha256.Sum256([]byte(strings.Join(lines, "\n")))

	return hex.EncodeToString(sum[:])
}

// FailureDomain returns the node's coarsest failure-domain key: its rack, or a
// node-specific key when the rack is unset so unlabeled nodes are never treated
// as sharing a rack (they degrade to node-level spread instead of falsely
// appearing rack-diverse).
func (n Node) FailureDomain() string {
	if n.Rack == "" {
		return "\x00node:" + string(n.ID)
	}

	return n.Rack
}
