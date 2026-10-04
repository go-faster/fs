package engine

import (
	"context"
	"encoding/json"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/block"
	"github.com/go-faster/fs/internal/cluster/layout"
	"github.com/go-faster/fs/internal/cluster/meta"
)

// A bucket's scheme is how new blocks of its objects are stored: replicated
// ("rf3", the default) or erasure coded ("ec:K,M"). Each block records the
// scheme it was written with, so changing a bucket's applies to new writes
// and leaves existing blocks as they are.

const settingScheme = "scheme"

// settingSchemeWidth is the widest K+M a bucket has been coded with: a
// bucket switched back to rf3 keeps the blocks it coded, and a layout must
// stay wide enough to read them.
const settingSchemeWidth = "scheme_width"

// schemeWidth is the width b's blocks need: its current scheme's, or the
// widest it ever had.
func schemeWidth(b meta.Bucket) int {
	var w int
	if s, ok := b.Settings[settingSchemeWidth]; ok {
		_ = json.Unmarshal(s.V, &w)
	}

	if s := bucketScheme(b); s.Coded() {
		w = max(w, s.K+s.M)
	}

	return w
}

// bucketScheme is the scheme b's new blocks are written with.
func bucketScheme(b meta.Bucket) block.Scheme {
	var v string
	if s, ok := b.Settings[settingScheme]; ok {
		_ = json.Unmarshal(s.V, &v)
	}

	sch, _ := block.ParseScheme(v)

	return sch
}

// BucketScheme returns the scheme of a bucket's new blocks: "rf3" or
// "ec:K,M".
func (e *Engine) BucketScheme(ctx context.Context, bucket string) (string, error) {
	b, _, err := e.bucket(ctx, bucket)
	if err != nil {
		return "", err
	}

	return bucketScheme(b).String(), nil
}

// SetBucketScheme sets the scheme of a bucket's new blocks. A coded scheme
// needs a layout with at least K+M nodes per partition, spread for that
// width, so that the shards land on distinct nodes; anything else is
// refused, never stored with less redundancy than asked.
func (e *Engine) SetBucketScheme(ctx context.Context, bucket, scheme string) error {
	sch, err := block.ParseScheme(scheme)
	if err != nil {
		return errors.Wrap(fs.ErrUnsupportedOperation, err.Error())
	}

	if sch.Coded() {
		l := e.member.Layout()
		if l == nil || len(l.Widths) == 0 || l.Widths[len(l.Widths)-1] < sch.K+sch.M {
			return errors.Wrapf(fs.ErrUnsupportedOperation,
				"%s needs a cluster layout spread for width %d (fs layout apply with that width)", sch, sch.K+sch.M)
		}
	}

	defer e.lock(bucketsPK, bucket)()

	b, _, err := e.bucket(ctx, bucket)
	if err != nil {
		return err
	}

	// The scheme, and the widest the bucket has ever needed, in one write.
	settings := map[string]meta.LWW[json.RawMessage]{
		settingScheme: {TS: meta.NextTS(e.ts(), b.Settings[settingScheme].TS), V: mustJSON(sch.String())},
	}

	if w := sch.K + sch.M; sch.Coded() && w > schemeWidth(b) {
		settings[settingSchemeWidth] = meta.LWW[json.RawMessage]{TS: meta.NextTS(e.ts(), b.Settings[settingSchemeWidth].TS), V: mustJSON(w)}
	}

	return e.buckets.Insert(ctx, bucketsPK, bucket, meta.Bucket{Settings: settings})
}

// getBlock reads one block as loc says it is stored, into buf when it can.
func (e *Engine) getBlock(ctx context.Context, loc blockLoc, buf []byte) ([]byte, error) {
	if loc.Scheme == "" {
		return e.blocks.GetInto(ctx, loc.Hash, buf)
	}

	sch, err := block.ParseScheme(loc.Scheme)
	if err != nil {
		return nil, err
	}

	return e.blocks.GetCoded(ctx, loc.Hash, int(loc.Size), sch)
}

// CheckLayout refuses a layout narrower than a bucket's erasure code, current
// or past: it would leave that bucket's new blocks nowhere to go and its coded
// blocks unreadable until widened again.
//
// Not being able to tell never blocks a layout change: before the first
// layout nothing is stored, and with the metadata unreachable a new layout
// may be how the cluster gets it back.
func (e *Engine) CheckLayout(ctx context.Context, l *layout.Layout) error {
	if e.member.Layout() == nil {
		return nil
	}

	buckets, err := e.ListBuckets(ctx)
	if err != nil {
		return nil //nolint:nilerr // Unknown is not a reason to refuse; see above.
	}

	wide := 0
	if len(l.Widths) > 0 {
		wide = l.Widths[len(l.Widths)-1]
	}

	for _, b := range buckets {
		row, _, err := e.bucket(ctx, b.Name)
		if err != nil {
			continue // Deleted meanwhile.
		}

		if need := schemeWidth(row); need > wide {
			return errors.Wrapf(fs.ErrUnsupportedOperation,
				"the layout spreads for width %d; bucket %q holds blocks coded %d wide — add it to widths", wide, b.Name, need)
		}
	}

	return nil
}
