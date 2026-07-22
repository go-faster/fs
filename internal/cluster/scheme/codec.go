package scheme

import (
	"bytes"

	"github.com/go-faster/errors"
	"github.com/klauspost/reedsolomon"
)

// Codec is a systematic Reed-Solomon encoder over GF(2^8) for k data + m parity
// shards (github.com/klauspost/reedsolomon). It is the single codec behind every
// scheme: EC RS(k,m) uses it directly, and RF=2.5's half-parity is RS(2,1) over
// the object's two halves — so both schemes share one implementation and one
// repair path (no bespoke XOR).
type Codec struct {
	k, m int
	enc  reedsolomon.Encoder
}

// NewCodec builds a Reed-Solomon codec with k data and m parity shards.
func NewCodec(k, m int) (*Codec, error) {
	if k < 1 {
		return nil, errors.Errorf("data shards k must be >= 1 (got %d)", k)
	}

	if m < 1 {
		return nil, errors.Errorf("parity shards m must be >= 1 (got %d)", m)
	}

	enc, err := reedsolomon.New(k, m)
	if err != nil {
		return nil, errors.Wrap(err, "new reed-solomon")
	}

	return &Codec{k: k, m: m, enc: enc}, nil
}

// Codec returns the Reed-Solomon codec for the scheme: RS(k,m) for EC and
// RS(2,1) for RF=2.5 (whose parity fragment is one RS parity shard over the two
// object halves). RF=3 stores whole replicas only and has no codec.
func (s Scheme) Codec() (*Codec, error) {
	switch s.Kind {
	case RF25:
		return NewCodec(2, 1)
	case EC:
		return NewCodec(s.K, s.M)
	case RF3:
		return nil, errors.New("rf3 stores full replicas and has no erasure codec")
	default:
		return nil, errors.Errorf("unknown scheme kind %d", s.Kind)
	}
}

// DataShards reports k. ParityShards reports m. TotalShards reports k+m.
func (c *Codec) DataShards() int   { return c.k }
func (c *Codec) ParityShards() int { return c.m }
func (c *Codec) TotalShards() int  { return c.k + c.m }

// Encode splits data into k equal, zero-padded data shards and computes m parity
// shards, returning all k+m shards (data shards first, then parity). Each shard
// is placed on a distinct target. data must be non-empty; the caller records the
// original length out of band (e.g. the object sidecar) for Reconstruct. The
// returned shards share no storage with data.
func (c *Codec) Encode(data []byte) ([][]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("cannot erasure-code an empty object")
	}

	shards, err := c.enc.Split(data)
	if err != nil {
		return nil, errors.Wrap(err, "split")
	}

	if err := c.enc.Encode(shards); err != nil {
		return nil, errors.Wrap(err, "encode parity")
	}

	return shards, nil
}

// Reconstruct rebuilds any missing shards (nil entries) in place — given at
// least k of the k+m present — then reassembles and returns the original object
// of origLen bytes. Present shards must all be the same length; pass nil for
// lost shards. Fewer than k present shards is an unrecoverable loss and errors.
func (c *Codec) Reconstruct(shards [][]byte, origLen int) ([]byte, error) {
	if len(shards) != c.k+c.m {
		return nil, errors.Errorf("expected %d shards, got %d", c.k+c.m, len(shards))
	}

	if err := c.enc.Reconstruct(shards); err != nil {
		return nil, errors.Wrap(err, "reconstruct")
	}

	var buf bytes.Buffer
	if err := c.enc.Join(&buf, shards, origLen); err != nil {
		return nil, errors.Wrap(err, "join")
	}

	return buf.Bytes(), nil
}

// Verify reports whether the parity shards are consistent with the data shards
// (all k+m shards must be present and equal length). Used by the scrubber/repair
// path to detect silent corruption.
func (c *Codec) Verify(shards [][]byte) (bool, error) {
	ok, err := c.enc.Verify(shards)
	if err != nil {
		return false, errors.Wrap(err, "verify")
	}

	return ok, nil
}
