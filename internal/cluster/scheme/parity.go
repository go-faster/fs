package scheme

import "github.com/go-faster/errors"

// RF=2.5 stores two full replicas plus a single parity fragment (on the third
// target). The parity is one Reed-Solomon parity shard of RS(2,1) over the
// object's two halves — the degenerate two-data case of Reed-Solomon, computed
// by the same codec as EC (not a bespoke XOR). It does not by itself rebuild a
// lost replica, but with the surviving half of a replica it repairs a damaged
// half at half the storage of a third full replica, and detects bit-rot.

// HalfParity returns the RF=2.5 parity fragment for data: the RS(2,1) parity
// shard over the object's two halves, of length ceil(len(data)/2). An empty
// object has an empty (nil) parity.
func HalfParity(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, nil
	}

	c, err := NewCodec(2, 1)
	if err != nil {
		return nil, err
	}

	shards, err := c.Encode(data)
	if err != nil {
		return nil, err
	}

	return shards[2], nil // shards[0], shards[1] are the halves; shards[2] is parity
}

// RepairHalf reconstructs a replica whose one half was lost or corrupted, from
// its surviving half and the parity fragment. survivingHalf is the intact half
// bytes; survivingIndex is 0 (first half) or 1 (second). parity is the fragment
// from HalfParity. origLen is the object's original length. It returns the fully
// reassembled object.
func RepairHalf(survivingHalf []byte, survivingIndex int, parity []byte, origLen int) ([]byte, error) {
	if survivingIndex != 0 && survivingIndex != 1 {
		return nil, errors.Errorf("survivingIndex must be 0 or 1 (got %d)", survivingIndex)
	}

	if len(survivingHalf) != len(parity) {
		return nil, errors.Errorf("half (%d) and parity (%d) length mismatch", len(survivingHalf), len(parity))
	}

	c, err := NewCodec(2, 1)
	if err != nil {
		return nil, err
	}

	// Shards: [half0, half1, parity]; the lost half stays nil for Reconstruct.
	shards := make([][]byte, 3)
	shards[survivingIndex] = survivingHalf
	shards[2] = parity

	return c.Reconstruct(shards, origLen)
}
