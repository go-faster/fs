package engine

import (
	"context"
	"encoding/json"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/block"
	"github.com/go-faster/fs/internal/cluster/meta"
)

// A bucket's scheme is how new blocks of its objects are stored: replicated
// ("rf3", the default) or erasure coded ("ec:K,M"). Each block records the
// scheme it was written with, so changing a bucket's applies to new writes
// and leaves existing blocks as they are.
//
// ponytail: no re-encoding of existing blocks; a background job if asked.

const settingScheme = "scheme"

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

	return e.setSetting(ctx, bucket, settingScheme, sch.String())
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
