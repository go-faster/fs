package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/cluster/meta"
	"github.com/go-faster/fs/internal/sse"
)

// RotateResult reports a key rotation.
type RotateResult struct {
	// Rewrapped counts data keys moved onto the current master key.
	Rewrapped int
	// Current counts data keys already under it.
	Current int
	// Failed names the versions and uploads whose key could not be moved —
	// most often because the master key it is under is not configured — by
	// bucket/key@version, up to MaxRotateFailures of them; Remaining counts
	// them all. A key cannot be retired while Remaining is not zero.
	Failed    []string
	Remaining int
}

// MaxRotateFailures bounds RotateResult.Failed.
const MaxRotateFailures = 100

// RotateKeys moves the data key of every encrypted version and upload onto
// the keyring's current master key. Only the keys are rewritten — a few dozen
// bytes per version — never the data. It can be interrupted and run again:
// keys already current are skipped.
//
// Each rewrapped key is a newer write of the version's Key register, so it
// merges with whatever else happens to the version meanwhile: a completed
// upload keeps it, a deleted version drops it.
func (e *Engine) RotateKeys(ctx context.Context) (RotateResult, error) {
	var res RotateResult

	if e.keyring == nil {
		return res, errors.Wrap(fs.ErrUnsupportedOperation, "no master key is configured")
	}

	buckets, err := e.ListBuckets(ctx)
	if err != nil {
		return res, err
	}

	for _, b := range buckets {
		_, inc, err := e.bucket(ctx, b.Name)
		if err != nil {
			continue // Deleted meanwhile.
		}

		start := ""

		for {
			page, err := e.objects.Range(ctx, inc.ID, start, 1000)
			if err != nil {
				return res, err
			}

			for _, r := range page {
				for _, v := range r.Row.Versions {
					if err := e.rotateVersion(ctx, inc.ID, r.SK, v, &res); err != nil {
						res.Remaining++
						if len(res.Failed) < MaxRotateFailures {
							res.Failed = append(res.Failed, fmt.Sprintf("%s/%s@%s: %v", b.Name, r.SK, v.ID, err))
						}
					}
				}
			}

			if len(page) < 1000 {
				break
			}

			start = page[len(page)-1].SK + "\x00"
		}
	}

	return res, nil
}

func (e *Engine) rotateVersion(ctx context.Context, bucketID, key string, v meta.Version, res *RotateResult) error {
	if v.State == meta.Gone || len(v.Key.V) == 0 {
		return nil
	}

	var w sse.WrappedKey
	if err := json.Unmarshal(v.Key.V, &w); err != nil {
		return errors.Wrap(err, "decode data key")
	}

	nw, changed, err := e.keyring.Rewrap(w)
	if err != nil {
		return err
	}

	if !changed {
		res.Current++

		return nil
	}

	v.Key = meta.LWW[json.RawMessage]{TS: meta.NextTS(e.ts(), v.Key.TS), V: mustJSON(nw)}

	if err := e.objects.Insert(ctx, bucketID, key, meta.Object{Versions: []meta.Version{v}}); err != nil {
		return err
	}

	res.Rewrapped++

	return nil
}
