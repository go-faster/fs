package meta

import (
	"bytes"
	"cmp"
	"encoding/json"
	"slices"
)

// State is where a version is in its life. States only move forward.
type State int

const (
	// Uploading: started, not complete. Not visible to reads.
	Uploading State = iota
	// Complete: the version's content (or delete marker) is final.
	Complete
	// Gone: aborted, or permanently deleted. A tombstone, kept so that a
	// replica that has not seen the delete cannot bring the version back.
	//
	// ponytail: tombstones are never collected; collect them once every
	// replica has merged them (#301).
	Gone
)

// Version is one version of an object.
type Version struct {
	// ID is the version's unique ID; the S3 version ID unless Null.
	ID string `json:"id"`
	// TS orders versions of a key; the newest complete one is current.
	TS int64 `json:"ts"`
	// Null marks the version written while versioning is off or suspended,
	// which S3 reports with version ID "null". A newer complete null version
	// replaces every older null version.
	Null  bool  `json:"null,omitempty"`
	State State `json:"state"`
	// Completed records that the version reached Complete, and stays set once
	// it is Gone. A null version that completed replaces older null versions
	// even after it is deleted; an aborted upload never did, and replaces
	// nothing.
	Completed bool `json:"completed,omitempty"`
	// DeleteMarker is a complete version that says the key is deleted.
	DeleteMarker bool `json:"delete_marker,omitempty"`
	// Payload is the engine's description of the version: headers, size,
	// ETag, where its bytes live. Set once, when the version completes.
	Payload json.RawMessage `json:"payload,omitempty"`
}

func (v Version) before(w Version) bool {
	if v.TS != w.TS {
		return v.TS < w.TS
	}

	return v.ID < w.ID
}

// mergeVersion merges two copies of the same version: the further state wins.
// Copies in the same state are the same write, but pick deterministically all
// the same.
func mergeVersion(a, b Version) Version {
	out := a

	switch {
	case b.State > a.State:
		out = b
	case b.State == a.State && bytes.Compare(encode(b), encode(a)) > 0:
		out = b
	}

	out.Completed = a.Completed || b.Completed || out.State == Complete

	return out
}

// Object is one key's versions, oldest first.
type Object struct {
	Versions []Version `json:"versions"`
}

// MergeObject is the objects table's merge: the union of both version lists,
// each version at its furthest state, without the null versions replaced by a
// newer null version that completed.
//
// Replacement is what keeps an unversioned bucket's row one version long. It
// is safe to merge because the replacing version is never itself dropped: it
// can only be replaced by a newer completed null version, which replaces
// everything it did.
func MergeObject(a, b Object) Object {
	byID := make(map[string]Version, len(a.Versions)+len(b.Versions))

	for _, v := range slices.Concat(a.Versions, b.Versions) {
		if v.State == Complete {
			v.Completed = true
		}

		if prev, ok := byID[v.ID]; ok {
			v = mergeVersion(prev, v)
		}

		if v.State == Gone {
			v.Payload, v.DeleteMarker = nil, false
		}

		byID[v.ID] = v
	}

	var latestNull *Version

	for _, v := range byID {
		if v.Null && v.Completed && (latestNull == nil || latestNull.before(v)) {
			latestNull = &v
		}
	}

	out := make([]Version, 0, len(byID))

	for _, v := range byID {
		if latestNull != nil && v.Null && v.before(*latestNull) {
			continue
		}

		out = append(out, v)
	}

	if len(out) == 0 {
		return Object{}
	}

	slices.SortFunc(out, func(x, y Version) int {
		return cmp.Or(cmp.Compare(x.TS, y.TS), cmp.Compare(x.ID, y.ID))
	})

	return Object{Versions: out}
}

// Current returns the version reads see: the newest complete one. ok is false
// when there is none, or when it is a delete marker — the key does not exist.
func (o Object) Current() (v Version, ok bool) {
	for i := len(o.Versions) - 1; i >= 0; i-- {
		if v := o.Versions[i]; v.State == Complete {
			return v, !v.DeleteMarker
		}
	}

	return Version{}, false
}

// Latest returns the newest version in any state, for a coordinator choosing
// the next timestamp; ok is false for an empty row.
func (o Object) Latest() (v Version, ok bool) {
	if len(o.Versions) == 0 {
		return Version{}, false
	}

	return o.Versions[len(o.Versions)-1], true
}

// Find returns the version with the given ID.
func (o Object) Find(id string) (Version, bool) {
	for _, v := range o.Versions {
		if v.ID == id {
			return v, true
		}
	}

	return Version{}, false
}
