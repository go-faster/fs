package meta

import "encoding/json"

// Incarnation is one life of a bucket name: created with a fresh ID, maybe
// later deleted. Recreating a name starts a new incarnation, whose objects
// live under the new ID.
type Incarnation struct {
	ID      string `json:"id"`
	Owner   string `json:"owner,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
}

// Bucket is the buckets table's row.
type Bucket struct {
	Incarnation LWW[Incarnation] `json:"incarnation"`
	// Settings are the bucket's subresources — versioning, ACL, CORS,
	// lifecycle and the like — each its own register, so changing one never
	// races another. The encoding of each is the engine's.
	Settings map[string]LWW[json.RawMessage] `json:"settings,omitempty"`
}

// MergeBucket is the buckets table's merge.
func MergeBucket(a, b Bucket) Bucket {
	out := Bucket{Incarnation: a.Incarnation.Merge(b.Incarnation)}

	if len(a.Settings)+len(b.Settings) == 0 {
		return out
	}

	out.Settings = make(map[string]LWW[json.RawMessage], len(a.Settings)+len(b.Settings))

	for k, v := range a.Settings {
		out.Settings[k] = v
	}

	for k, v := range b.Settings {
		if prev, ok := out.Settings[k]; ok {
			v = prev.Merge(v)
		}

		out.Settings[k] = v
	}

	return out
}

// Live reports whether the bucket exists, and its incarnation.
func (b Bucket) Live() (Incarnation, bool) {
	inc := b.Incarnation.V

	return inc, inc.ID != "" && !inc.Deleted
}
