package meta

import (
	"fmt"
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/go-faster/fs/internal/cluster/table"
)

// randomOutbox is an outbox row a replica can hold: the merge of some
// changes and deliveries.
func randomOutbox(r *rand.Rand) Outbox {
	var o Outbox

	for range r.IntN(4) {
		w := Outbox{Pending: r.Int64N(4), Version: r.IntN(2) == 0, Size: r.Int64N(3)}
		for i := range 3 {
			if r.IntN(2) == 0 {
				w.Targets = append(w.Targets, fmt.Sprint("t", i))
			}
		}

		if r.IntN(2) == 0 {
			w.Shipped = map[string]int64{fmt.Sprint("t", r.IntN(3)): r.Int64N(4)}
		}

		if r.IntN(2) == 0 {
			w.Refused = map[string]Refusal{fmt.Sprint("t", r.IntN(3)): {
				Pending: r.Int64N(4), At: r.Int64N(2), Reason: fmt.Sprint("r", r.IntN(2)),
			}}
		}

		o = MergeOutbox(o, w)
	}

	return o
}

func TestOutboxMergeLaws(t *testing.T) { lawsHold(t, MergeOutbox, randomOutbox) }

func TestOutboxPending(t *testing.T) {
	o := Outbox{Targets: []string{"a", "b"}, Pending: 5, Version: true}
	assert.True(t, o.PendingFor("a"))
	assert.False(t, o.PendingFor("c"), "not a target of the row")

	// A delivery and a later change land in either order.
	shipped := Outbox{Shipped: map[string]int64{"a": 5}}
	changed := Outbox{Targets: []string{"a", "b"}, Pending: 7}

	for _, got := range []Outbox{
		MergeOutbox(MergeOutbox(o, shipped), changed),
		MergeOutbox(MergeOutbox(o, changed), shipped),
	} {
		assert.True(t, got.PendingFor("a"), "the change after the delivery is still to ship")
		assert.True(t, got.Version)
		assert.False(t, got.Done())
	}

	done := MergeOutbox(o, Outbox{Shipped: map[string]int64{"a": 5, "b": 9}})
	assert.False(t, done.PendingFor("a"))
	assert.False(t, done.PendingFor("b"))
	assert.True(t, done.Done())

	_, c := CompactOutbox(done)
	assert.Equal(t, table.Delete, c)

	_, c = CompactOutbox(o)
	assert.Equal(t, table.Keep, c)
}

func TestOutboxRefused(t *testing.T) {
	o := Outbox{Targets: []string{"a"}, Pending: 5}

	refused := MergeOutbox(o, Outbox{Refused: map[string]Refusal{"a": {Pending: 5, At: 1, Reason: "no"}}})
	r, ok := refused.RefusedBy("a")
	assert.True(t, ok)
	assert.Equal(t, "no", r.Reason)
	assert.True(t, refused.PendingFor("a"), "a refused change is still to ship")

	// A later change, or a delivery, ends the refusal.
	_, ok = MergeOutbox(refused, Outbox{Pending: 6}).RefusedBy("a")
	assert.False(t, ok)

	_, ok = MergeOutbox(refused, Outbox{Shipped: map[string]int64{"a": 5}}).RefusedBy("a")
	assert.False(t, ok)

	// The later refusal wins, whatever the order.
	again := Outbox{Refused: map[string]Refusal{"a": {Pending: 5, At: 2, Reason: "still no"}}}
	assert.Equal(t, MergeOutbox(refused, again), MergeOutbox(again, refused))
	r, _ = MergeOutbox(again, refused).RefusedBy("a")
	assert.Equal(t, "still no", r.Reason)
}
