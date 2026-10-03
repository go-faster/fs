package meta

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lawsHold checks that merge is commutative, associative and idempotent over
// rows gen produces, comparing encodings.
func lawsHold[R any](t *testing.T, merge func(a, b R) R, gen func(*rand.Rand) R) {
	t.Helper()

	r := rand.New(rand.NewPCG(1, 2))
	eq := func(msg string, a, b R) {
		t.Helper()
		require.JSONEq(t, string(encode(a)), string(encode(b)), msg)
	}

	for range 2000 {
		a, b, c := gen(r), gen(r), gen(r)

		eq("commutative", merge(a, b), merge(b, a))
		eq("associative", merge(merge(a, b), c), merge(a, merge(b, c)))
		eq("idempotent", a, merge(a, a))
	}
}

// writes is a small universe of version writes. A version ID always has the
// same timestamp and null flag, as it does when one coordinator mints it.
func randomWrite(r *rand.Rand) Version {
	id := r.IntN(6)
	v := Version{ID: fmt.Sprint("v", id), TS: int64(id), Null: id%2 == 0, State: State(r.IntN(3))}

	if v.State == Complete {
		v.DeleteMarker = r.IntN(4) == 0
		if !v.DeleteMarker {
			v.Payload = json.RawMessage(fmt.Sprintf(`{"etag":"%d"}`, id))
		}
	}

	if v.State == Gone {
		v.Completed = r.IntN(2) == 0
	}

	if v.State != Gone && r.IntN(2) == 0 {
		v.Attrs = LWW[json.RawMessage]{TS: r.Int64N(3), V: json.RawMessage(fmt.Sprintf(`{"tags":%d}`, r.IntN(3)))}
	}

	if v.State != Gone && r.IntN(2) == 0 {
		v.Key = LWW[json.RawMessage]{TS: r.Int64N(3), V: json.RawMessage(fmt.Sprintf(`{"k":%d}`, r.IntN(3)))}
	}

	return v
}

// randomObject is a row a replica can hold: the merge of some writes.
func randomObject(r *rand.Rand) Object {
	var o Object
	for range r.IntN(4) {
		o = MergeObject(o, Object{Versions: []Version{randomWrite(r)}})
	}

	return o
}

func TestObjectMergeLaws(t *testing.T) { lawsHold(t, MergeObject, randomObject) }

// TestObjectConverges delivers the same writes to replicas in different
// orders, some through intermediate replicas, some twice: every replica must
// end on the same row.
func TestObjectConverges(t *testing.T) {
	r := rand.New(rand.NewPCG(3, 4))

	for range 500 {
		writes := make([]Object, 1+r.IntN(8))
		for i := range writes {
			writes[i] = Object{Versions: []Version{randomWrite(r)}}
		}

		var want Object
		for _, w := range writes {
			want = MergeObject(want, w)
		}

		for range 5 {
			// Two replicas each see a random subset, possibly with repeats,
			// then exchange; a third gets everything in shuffled order.
			var a, b Object

			for _, i := range r.Perm(len(writes)) {
				switch r.IntN(3) {
				case 0:
					a = MergeObject(a, writes[i])
				case 1:
					b = MergeObject(b, writes[i])
				default:
					a = MergeObject(a, writes[i])
					b = MergeObject(writes[i], b)
				}
			}

			for _, i := range r.Perm(len(writes)) {
				a = MergeObject(a, writes[i])
			}

			got := MergeObject(b, a)
			require.JSONEq(t, string(encode(want)), string(encode(got)))
		}
	}
}

func row(vs ...Version) Object {
	var o Object
	for _, v := range vs {
		o = MergeObject(o, Object{Versions: []Version{v}})
	}

	return o
}

func TestUnversionedOverwrite(t *testing.T) {
	o := row(
		Version{ID: "a", TS: 1, Null: true, State: Complete, Payload: json.RawMessage(`1`)},
		Version{ID: "b", TS: 2, Null: true, State: Complete, Payload: json.RawMessage(`2`)},
	)

	require.Len(t, o.Versions, 1, "a newer null version replaces the older one")

	cur, ok := o.Current()
	require.True(t, ok)
	assert.Equal(t, "b", cur.ID)
}

func TestVersionedKeepsHistory(t *testing.T) {
	o := row(
		Version{ID: "a", TS: 1, State: Complete},
		Version{ID: "b", TS: 2, State: Complete},
		Version{ID: "c", TS: 3, State: Complete, DeleteMarker: true},
	)

	require.Len(t, o.Versions, 3)

	_, ok := o.Current()
	assert.False(t, ok, "a delete marker on top hides the key")

	o = MergeObject(o, row(Version{ID: "c", TS: 3, State: Gone}))
	cur, ok := o.Current()
	require.True(t, ok, "removing the marker brings back the version below")
	assert.Equal(t, "b", cur.ID)
}

func TestUploadsAreInvisible(t *testing.T) {
	o := row(
		Version{ID: "a", TS: 1, Null: true, State: Complete},
		Version{ID: "b", TS: 2, Null: true, State: Uploading},
	)

	cur, ok := o.Current()
	require.True(t, ok)
	assert.Equal(t, "a", cur.ID, "an upload in flight does not replace the current version")

	o = MergeObject(o, row(Version{ID: "b", TS: 2, Null: true, State: Gone}))
	cur, ok = o.Current()
	require.True(t, ok, "an aborted upload deletes nothing")
	assert.Equal(t, "a", cur.ID)
}

func TestDeletedNullStaysReplaced(t *testing.T) {
	// The older null version must not return when the newer one is deleted,
	// on a replica that pruned it or one that never did.
	older := Version{ID: "a", TS: 1, Null: true, State: Complete}
	newer := Version{ID: "b", TS: 2, Null: true, State: Complete}
	// The deleting coordinator read b, so its write says b had completed.
	deleted := Version{ID: "b", TS: 2, Null: true, State: Gone, Completed: true}

	pruned := MergeObject(row(older, newer), row(deleted))
	stale := MergeObject(row(older), row(deleted))

	require.JSONEq(t, string(encode(pruned)), string(encode(stale)))

	_, ok := stale.Current()
	assert.False(t, ok)
}

func TestGoneDropsPayload(t *testing.T) {
	o := row(
		Version{ID: "a", TS: 1, State: Complete, Payload: json.RawMessage(`{"big":true}`)},
		Version{ID: "a", TS: 1, State: Gone},
	)

	require.Len(t, o.Versions, 1)
	assert.Equal(t, Gone, o.Versions[0].State)
	assert.Nil(t, o.Versions[0].Payload)
	assert.True(t, o.Versions[0].Completed)
}

func TestFindLatest(t *testing.T) {
	o := row(Version{ID: "a", TS: 1, State: Complete}, Version{ID: "b", TS: 5, State: Uploading})

	v, ok := o.Find("a")
	require.True(t, ok)
	assert.Equal(t, int64(1), v.TS)

	_, ok = o.Find("zzz")
	assert.False(t, ok)

	latest, ok := o.Latest()
	require.True(t, ok)
	assert.Equal(t, "b", latest.ID)

	_, ok = Object{}.Latest()
	assert.False(t, ok)
}

func randomBucket(r *rand.Rand) Bucket {
	b := Bucket{Incarnation: LWW[Incarnation]{
		TS: r.Int64N(3),
		V:  Incarnation{ID: fmt.Sprint("id", r.IntN(3)), Deleted: r.IntN(2) == 0},
	}}

	for range r.IntN(3) {
		if b.Settings == nil {
			b.Settings = map[string]LWW[json.RawMessage]{}
		}

		b.Settings[fmt.Sprint("k", r.IntN(3))] = LWW[json.RawMessage]{
			TS: r.Int64N(3),
			V:  json.RawMessage(fmt.Sprint(r.IntN(3))),
		}
	}

	return b
}

func TestBucketMergeLaws(t *testing.T) { lawsHold(t, MergeBucket, randomBucket) }

func TestBucketRecreate(t *testing.T) {
	first := Bucket{Incarnation: LWW[Incarnation]{TS: 1, V: Incarnation{ID: "one"}}}
	deleted := Bucket{Incarnation: LWW[Incarnation]{TS: 2, V: Incarnation{ID: "one", Deleted: true}}}
	again := Bucket{Incarnation: LWW[Incarnation]{TS: 3, V: Incarnation{ID: "two"}}}

	b := MergeBucket(MergeBucket(first, again), deleted)

	inc, ok := b.Live()
	require.True(t, ok)
	assert.Equal(t, "two", inc.ID, "a recreated bucket has a new ID, so it starts empty")

	_, ok = MergeBucket(first, deleted).Live()
	assert.False(t, ok)
}

func TestBucketSettingsIndependent(t *testing.T) {
	a := Bucket{Settings: map[string]LWW[json.RawMessage]{"versioning": {TS: 5, V: json.RawMessage(`"Enabled"`)}}}
	b := Bucket{Settings: map[string]LWW[json.RawMessage]{"cors": {TS: 1, V: json.RawMessage(`[]`)}}}

	m := MergeBucket(a, b)
	assert.Len(t, m.Settings, 2, "a newer change to one setting does not drop another")
}

func TestBlockRefMergeLaws(t *testing.T) {
	lawsHold(t, MergeBlockRef, func(r *rand.Rand) BlockRef { return BlockRef{Deleted: r.IntN(2) == 0} })
}

func TestLWW(t *testing.T) {
	a, b := LWW[string]{TS: 1, V: "z"}, LWW[string]{TS: 2, V: "a"}
	assert.Equal(t, b, a.Merge(b), "later wins")

	x, y := LWW[string]{TS: 1, V: "x"}, LWW[string]{TS: 1, V: "y"}
	assert.Equal(t, x.Merge(y), y.Merge(x), "a tie is broken the same way everywhere")

	assert.Equal(t, int64(10), NextTS(10, 3))
	assert.Equal(t, int64(4), NextTS(2, 3), "a clock behind the row still orders after it")
}

func TestKeyRewrapSurvivesCompletion(t *testing.T) {
	// A rotation rewraps an upload's key while another node completes it
	// with the key it read: the completed version keeps the rewrapped key.
	upload := Version{ID: "u", TS: 1, State: Uploading, Payload: json.RawMessage(`{"up":1}`)}
	upload.Key = LWW[json.RawMessage]{TS: 1, V: json.RawMessage(`"old"`)}

	rotated := upload
	rotated.Key = LWW[json.RawMessage]{TS: 2, V: json.RawMessage(`"new"`)}

	done := upload
	done.TS, done.State, done.Payload = 3, Complete, json.RawMessage(`{"etag":"x"}`)

	o := MergeObject(row(rotated), row(done))
	require.Len(t, o.Versions, 1)
	assert.Equal(t, Complete, o.Versions[0].State)
	assert.JSONEq(t, `"new"`, string(o.Versions[0].Key.V))
}

func TestAttrsMergeApart(t *testing.T) {
	// Tags changed on two replicas of one complete version: the later change
	// wins, whatever else the copies agree on.
	base := Version{ID: "a", TS: 1, State: Complete, Payload: json.RawMessage(`{"etag":"x"}`)}

	older, newer := base, base
	older.Attrs = LWW[json.RawMessage]{TS: 5, V: json.RawMessage(`{"tags":"old"}`)}
	newer.Attrs = LWW[json.RawMessage]{TS: 9, V: json.RawMessage(`{"tags":"new"}`)}

	o := MergeObject(row(older), row(newer))
	require.Len(t, o.Versions, 1)
	assert.JSONEq(t, `{"tags":"new"}`, string(o.Versions[0].Attrs.V))
	assert.JSONEq(t, `{"etag":"x"}`, string(o.Versions[0].Payload))
}
