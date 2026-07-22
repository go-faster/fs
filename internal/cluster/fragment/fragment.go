// Package fragment is the connective layer between placement and the erasure
// schemes: it encodes an object into the per-target fragments a scheme stores,
// and reconstructs the object from whatever fragments survive. It is the pure
// core of clusterstore's write and read paths — deterministic, no I/O, no
// networking — so the whole store/reconstruct cycle is unit-testable without a
// cluster.
package fragment

import (
	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/placement"
	"github.com/go-faster/fs/internal/cluster/scheme"
)

// Kind labels what a fragment holds.
type Kind uint8

const (
	// Replica is a full object copy (RF=2.5 and RF=3).
	Replica Kind = iota
	// Parity is the RF=2.5 half-parity fragment (Reed-Solomon RS(2,1) over the
	// object's two halves), stored on the third target.
	Parity
	// Shard is an EC shard: a data shard when Index < k, a parity shard
	// otherwise.
	Shard
)

// Fragment is one scheme output bound to one placement target: the bytes that
// target must store and enough metadata to place it back for reconstruction.
type Fragment struct {
	// Target is the physical destination (node + disk).
	Target placement.Target
	// Kind is what the fragment holds.
	Kind Kind
	// Index is the fragment's position: the replica ordinal for RF schemes, or
	// the shard index (0..k+m-1) for EC. It is how Decode slots a survivor back
	// into the codec.
	Index int
	// Data is the fragment payload (a full object for Replica, the parity
	// fragment for Parity, or a shard for Shard).
	Data []byte
}

// ErrInsufficientTargets means the topology cannot provide the distinct targets
// the scheme requires (e.g. RS(4,2) needs 6 but only 4 usable disks exist). The
// write must be refused rather than silently under-protected.
var ErrInsufficientTargets = errors.New("insufficient targets for scheme")

// ErrUnrecoverable means too many fragments were lost to reconstruct the object
// (below one surviving replica, or below k EC shards).
var ErrUnrecoverable = errors.New("too many fragments lost to reconstruct object")

// Encode plans a write: it places the scheme's targets for key over the
// topology and produces the fragment each target must store. The returned
// fragments are ordered by target (primary first). The caller records the
// object's original length out of band (the sidecar) for Decode. It returns
// ErrInsufficientTargets when the cluster cannot host the scheme.
func Encode(topo *cluster.Topology, s scheme.Scheme, key string, data []byte) ([]Fragment, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}

	targets := placement.Place(topo, key, s.Copies())
	if len(targets) < s.Copies() {
		return nil, errors.Wrapf(ErrInsufficientTargets, "need %d, got %d", s.Copies(), len(targets))
	}

	switch s.Kind {
	case scheme.RF3:
		return replicas(targets, data), nil
	case scheme.RF25:
		frags := replicas(targets[:2], data)

		parity, err := scheme.HalfParity(data)
		if err != nil {
			return nil, err
		}

		return append(frags, Fragment{Target: targets[2], Kind: Parity, Index: 2, Data: parity}), nil
	case scheme.EC:
		return ecShards(targets, s, data)
	default:
		return nil, errors.Errorf("unknown scheme kind %d", s.Kind)
	}
}

// replicas builds one full-copy fragment per target.
func replicas(targets []placement.Target, data []byte) []Fragment {
	frags := make([]Fragment, len(targets))
	for i, t := range targets {
		frags[i] = Fragment{Target: t, Kind: Replica, Index: i, Data: data}
	}

	return frags
}

// ecShards splits data into k+m shards and binds each to a target.
func ecShards(targets []placement.Target, s scheme.Scheme, data []byte) ([]Fragment, error) {
	total := s.K + s.M

	// An empty object needs no coding: every shard is empty. Decode short-
	// circuits on origLen == 0.
	if len(data) == 0 {
		frags := make([]Fragment, total)
		for i, t := range targets {
			frags[i] = Fragment{Target: t, Kind: Shard, Index: i}
		}

		return frags, nil
	}

	codec, err := s.Codec()
	if err != nil {
		return nil, err
	}

	shards, err := codec.Encode(data)
	if err != nil {
		return nil, err
	}

	frags := make([]Fragment, total)
	for i, t := range targets {
		frags[i] = Fragment{Target: t, Kind: Shard, Index: i, Data: shards[i]}
	}

	return frags, nil
}

// Decode reconstructs the object from the fragments that survived (a subset of
// what Encode produced), per scheme. origLen is the object's original length.
//
//   - RF=2.5 / RF=3: any surviving full replica is returned directly. (Rebuilding
//     a partially-damaged replica half from parity + the other half is the
//     repair worker's job, via scheme.RepairHalf — not the read path.)
//   - EC: any k of the k+m shards reconstruct the object.
//
// It returns ErrUnrecoverable when too few fragments survive.
func Decode(s scheme.Scheme, origLen int, frags []Fragment) ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}

	switch s.Kind {
	case scheme.RF25, scheme.RF3:
		for _, f := range frags {
			if f.Kind == Replica {
				return f.Data, nil
			}
		}

		return nil, errors.Wrap(ErrUnrecoverable, "no surviving replica")
	case scheme.EC:
		return decodeEC(s, origLen, frags)
	default:
		return nil, errors.Errorf("unknown scheme kind %d", s.Kind)
	}
}

// decodeEC gathers the surviving shards by index and reconstructs the object.
func decodeEC(s scheme.Scheme, origLen int, frags []Fragment) ([]byte, error) {
	if origLen == 0 {
		return []byte{}, nil
	}

	total := s.K + s.M
	shards := make([][]byte, total)
	present := 0

	for _, f := range frags {
		if f.Kind != Shard || f.Index < 0 || f.Index >= total {
			continue
		}

		if shards[f.Index] == nil {
			present++
		}

		shards[f.Index] = f.Data
	}

	if present < s.K {
		return nil, errors.Wrapf(ErrUnrecoverable, "have %d shards, need %d", present, s.K)
	}

	codec, err := s.Codec()
	if err != nil {
		return nil, err
	}

	return codec.Reconstruct(shards, origLen)
}
