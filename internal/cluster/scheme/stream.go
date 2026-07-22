package scheme

import (
	"io"

	"github.com/go-faster/errors"
)

// Streaming counterparts to the in-memory codec. They process shards in
// fixed-size blocks (the reedsolomon stream implementation), so an object is
// never fully buffered in memory — required for large parts, where a whole
// object (or even a single shard) may not fit in RAM.

// ShardSize is the exact per-shard payload size for an object of objSize
// bytes: ceil(objSize/k), the last data shard zero-padded to it. Zero for an
// empty object.
func (c *Codec) ShardSize(objSize int64) int64 {
	if objSize <= 0 {
		return 0
	}

	return (objSize + int64(c.k) - 1) / int64(c.k)
}

// SplitStream splits exactly size bytes from data into k equal-size shards
// written to dst (the last shard zero-padded). It is the streaming counterpart
// of Encode's split step; parity is computed separately by EncodeStream from
// re-readable shard storage.
func (c *Codec) SplitStream(data io.Reader, dst []io.Writer, size int64) error {
	if len(dst) != c.k {
		return errors.Errorf("expected %d data shard writers, got %d", c.k, len(dst))
	}

	if err := c.stream.Split(data, dst, size); err != nil {
		return errors.Wrap(err, "split stream")
	}

	return nil
}

// EncodeStream computes the m parity shards from the k data shard readers.
// Every reader must supply the same number of bytes (the ShardSize), which is
// what SplitStream wrote.
func (c *Codec) EncodeStream(data []io.Reader, parity []io.Writer) error {
	if len(data) != c.k {
		return errors.Errorf("expected %d data shard readers, got %d", c.k, len(data))
	}

	if len(parity) != c.m {
		return errors.Errorf("expected %d parity shard writers, got %d", c.m, len(parity))
	}

	if err := c.stream.Encode(data, parity); err != nil {
		return errors.Wrap(err, "encode stream")
	}

	return nil
}

// ReconstructStream rebuilds missing shards: valid holds readers for present
// shards and nil for lost ones; fill holds a writer at every index to rebuild
// (nil elsewhere). At least k shards must be present.
func (c *Codec) ReconstructStream(valid []io.Reader, fill []io.Writer) error {
	if len(valid) != c.k+c.m || len(fill) != c.k+c.m {
		return errors.Errorf("expected %d valid/fill entries, got %d/%d", c.k+c.m, len(valid), len(fill))
	}

	if err := c.stream.Reconstruct(valid, fill); err != nil {
		return errors.Wrap(err, "reconstruct stream")
	}

	return nil
}

// JoinStream reassembles the original object (origLen bytes, dropping shard
// padding) from the k data shard readers into dst.
func (c *Codec) JoinStream(dst io.Writer, data []io.Reader, origLen int64) error {
	if len(data) < c.k {
		return errors.Errorf("expected at least %d data shard readers, got %d", c.k, len(data))
	}

	if err := c.stream.Join(dst, data, origLen); err != nil {
		return errors.Wrap(err, "join stream")
	}

	return nil
}

// VerifyStream reports whether the parity shards are consistent with the data
// shards; all k+m shard readers must be supplied.
func (c *Codec) VerifyStream(shards []io.Reader) (bool, error) {
	if len(shards) != c.k+c.m {
		return false, errors.Errorf("expected %d shard readers, got %d", c.k+c.m, len(shards))
	}

	ok, err := c.stream.Verify(shards)
	if err != nil {
		return false, errors.Wrap(err, "verify stream")
	}

	return ok, nil
}

// HalfParityStream is the streaming HalfParity: it computes the RF=2.5 parity
// fragment (ceil(objSize/2) bytes, written to parity) from two readers over the
// stored object — first delivering bytes [0, ceil(n/2)) and second delivering
// [ceil(n/2), n). The caller opens the staged replica twice and positions the
// second reader at the half boundary; the second half is zero-padded
// internally. A no-op for an empty object.
func HalfParityStream(first, second io.Reader, objSize int64, parity io.Writer) error {
	if objSize <= 0 {
		return nil
	}

	c, err := NewCodec(2, 1)
	if err != nil {
		return err
	}

	half := (objSize + 1) / 2
	h1 := io.LimitReader(first, half)
	h2 := padTo(io.LimitReader(second, objSize-half), half)

	return c.EncodeStream([]io.Reader{h1, h2}, []io.Writer{parity})
}

// RepairHalfStream is the streaming RepairHalf: it rebuilds the lost half of a
// replica from the surviving half and the parity fragment, writing the rebuilt
// half (ceil(objSize/2) bytes, zero-padded when it is the second half) to
// rebuilt. survivingIndex is 0 when the first half survived, 1 for the second.
func RepairHalfStream(surviving io.Reader, survivingIndex int, parity io.Reader, objSize int64, rebuilt io.Writer) error {
	if survivingIndex != 0 && survivingIndex != 1 {
		return errors.Errorf("survivingIndex must be 0 or 1 (got %d)", survivingIndex)
	}

	if objSize <= 0 {
		return nil
	}

	c, err := NewCodec(2, 1)
	if err != nil {
		return err
	}

	half := (objSize + 1) / 2

	// Normalize the surviving half to exactly half bytes (the second half is
	// shorter on odd sizes and needs the zero pad restored).
	var sr io.Reader
	if survivingIndex == 0 {
		sr = io.LimitReader(surviving, half)
	} else {
		sr = padTo(io.LimitReader(surviving, objSize-half), half)
	}

	valid := make([]io.Reader, 3)
	valid[survivingIndex] = sr
	valid[2] = io.LimitReader(parity, half)

	fill := make([]io.Writer, 3)
	fill[1-survivingIndex] = rebuilt

	return c.ReconstructStream(valid, fill)
}

// zeroReader yields an endless stream of zero bytes.
type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// padTo returns a reader that delivers exactly n bytes: everything r yields
// (which must be at most n) followed by zeros.
func padTo(r io.Reader, n int64) io.Reader {
	return &padReader{r: r, remaining: n}
}

// padReader delivers its inner reader's bytes, then zero-fills up to the fixed
// total length.
type padReader struct {
	r         io.Reader
	remaining int64
	inner     bool // inner reader exhausted
}

func (p *padReader) Read(b []byte) (int, error) {
	if p.remaining <= 0 {
		return 0, io.EOF
	}

	if int64(len(b)) > p.remaining {
		b = b[:p.remaining]
	}

	if !p.inner {
		n, err := p.r.Read(b)
		if n > 0 {
			p.remaining -= int64(n)
			return n, nil
		}

		switch {
		case errors.Is(err, io.EOF):
			p.inner = true
		case err != nil:
			return n, err
		default:
			// Zero-byte read without error: try again next call.
			return 0, nil
		}
	}

	clear(b)
	p.remaining -= int64(len(b))

	return len(b), nil
}
