// Package scheme models the go-faster/fs replication and erasure schemes
// (DESIGN.md FR-17) and implements the pure codecs they need. A Scheme decides
// what each placement target holds: a full replica, an XOR half-parity
// fragment, or a Reed-Solomon shard. It is deterministic, does no I/O, and has
// no external dependencies.
//
// Three families, all sharing the same placement/quorum/repair machinery and
// differing only in what the non-primary targets store:
//
//   - RF=2.5 (default): full replicas on the first two targets, an XOR
//     half-parity fragment on the third. 2.5× storage. Survives any single
//     failure-domain loss and partial/bit-rot damage on either replica; does
//     NOT survive simultaneous total loss of both full replicas.
//   - RF=3: a full replica on all three targets. 3× storage. Survives any two
//     failure-domain losses once the third replica has landed.
//   - EC RS(k,m): k data shards + m parity shards across k+m targets.
//     (k+m)/k× storage, tolerating any m lost shards. The shard codec itself
//     is a follow-up; this package models the scheme (counts, tolerance,
//     overhead, parse/format) so placement and config can already reason about
//     it.
package scheme

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/go-faster/errors"
)

// Kind is a replication/erasure scheme family.
type Kind uint8

const (
	// RF25 is 2 full replicas plus an XOR half-parity fragment (2.5× storage).
	RF25 Kind = iota
	// RF3 is 3 full replicas (3× storage).
	RF3
	// EC is systematic Reed-Solomon with K data + M parity shards.
	EC
)

// Scheme is a fully-resolved replication scheme. K and M are meaningful only
// for EC and are zero otherwise.
type Scheme struct {
	Kind Kind
	K    int // EC: data shards
	M    int // EC: parity shards
}

// Default is the cluster-wide default scheme (RF=2.5).
var Default = Scheme{Kind: RF25}

// DefaultEC is the default erasure profile, RS(4,2) — 1.5× storage tolerating
// any two lost shards.
var DefaultEC = Scheme{Kind: EC, K: 4, M: 2}

// Validate reports whether the scheme is well-formed.
func (s Scheme) Validate() error {
	switch s.Kind {
	case RF25, RF3:
		if s.K != 0 || s.M != 0 {
			return errors.Errorf("replica scheme must not set k/m (got k=%d m=%d)", s.K, s.M)
		}

		return nil
	case EC:
		if s.K < 1 {
			return errors.Errorf("EC data shards k must be >= 1 (got %d)", s.K)
		}

		if s.M < 1 {
			return errors.Errorf("EC parity shards m must be >= 1 (got %d)", s.M)
		}

		return nil
	default:
		return errors.Errorf("unknown scheme kind %d", s.Kind)
	}
}

// Copies is the number of physical targets (distinct disks / failure domains)
// the scheme places: 3 for the replica schemes ([A, B, C]) or k+m for EC. This
// is the count to pass to placement.Place.
func (s Scheme) Copies() int {
	switch s.Kind {
	case RF25, RF3:
		return 3
	case EC:
		return s.K + s.M
	default:
		return 0
	}
}

// FullReplicas is the number of targets holding a complete object copy: 2 for
// RF=2.5, 3 for RF=3, 0 for EC (EC stores shards, not whole copies).
func (s Scheme) FullReplicas() int {
	switch s.Kind {
	case RF25:
		return 2
	case RF3:
		return 3
	default:
		return 0
	}
}

// WriteQuorum is the number of synchronous copies that must be durable before a
// write is acknowledged. The replica schemes ack after W=2 full replicas (the
// third target — parity or the RF=3 third replica — is written async behind the
// repair queue); EC must place all k+m shards synchronously (any fewer than k+m
// leaves it under-protected with no async path to complete a shard set safely).
func (s Scheme) WriteQuorum() int {
	switch s.Kind {
	case RF25, RF3:
		return 2
	case EC:
		return s.K + s.M
	default:
		return 0
	}
}

// Tolerance is the number of failure domains that can be lost, once the object
// is fully written, without data loss: 1 for RF=2.5, 2 for RF=3, m for EC.
func (s Scheme) Tolerance() int {
	switch s.Kind {
	case RF25:
		return 1
	case RF3:
		return 2
	case EC:
		return s.M
	default:
		return 0
	}
}

// Overhead is the storage amplification factor: 2.5 for RF=2.5, 3.0 for RF=3,
// (k+m)/k for EC.
func (s Scheme) Overhead() float64 {
	switch s.Kind {
	case RF25:
		return 2.5
	case RF3:
		return 3.0
	case EC:
		return float64(s.K+s.M) / float64(s.K)
	default:
		return 0
	}
}

// String renders the scheme in the config/CLI form: "rf2.5", "rf3" or "ec:k,m".
func (s Scheme) String() string {
	switch s.Kind {
	case RF25:
		return "rf2.5"
	case RF3:
		return "rf3"
	case EC:
		return fmt.Sprintf("ec:%d,%d", s.K, s.M)
	default:
		return fmt.Sprintf("scheme(%d)", s.Kind)
	}
}

// Parse reads a scheme from its config/CLI form: "rf2.5", "rf3" or "ec:k,m".
func Parse(s string) (Scheme, error) {
	switch strings.TrimSpace(strings.ToLower(s)) {
	case "rf2.5", "rf2_5":
		return Scheme{Kind: RF25}, nil
	case "rf3":
		return Scheme{Kind: RF3}, nil
	}

	rest, ok := strings.CutPrefix(strings.TrimSpace(strings.ToLower(s)), "ec:")
	if !ok {
		return Scheme{}, errors.Errorf("invalid scheme %q (want rf2.5, rf3 or ec:k,m)", s)
	}

	k, m, ok := strings.Cut(rest, ",")
	if !ok {
		return Scheme{}, errors.Errorf("invalid EC scheme %q (want ec:k,m)", s)
	}

	kv, err := strconv.Atoi(strings.TrimSpace(k))
	if err != nil {
		return Scheme{}, errors.Wrapf(err, "EC data shards in %q", s)
	}

	mv, err := strconv.Atoi(strings.TrimSpace(m))
	if err != nil {
		return Scheme{}, errors.Wrapf(err, "EC parity shards in %q", s)
	}

	out := Scheme{Kind: EC, K: kv, M: mv}
	if err := out.Validate(); err != nil {
		return Scheme{}, err
	}

	return out, nil
}
