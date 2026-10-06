package table

import (
	"fmt"
	"sync"
	"sync/atomic"

	"go.etcd.io/bbolt"
)

// Group commit: one goroutine per database applies writes, each transaction
// taking every write that queued while the one before it committed. A lone
// writer commits at once; under load a commit carries hundreds of writes,
// so throughput grows with load instead of capping at one commit at a time.
//
// bbolt's own Batch does not do this with a zero MaxBatchDelay — its timer
// fires at once and every call becomes its own transaction — and with any
// other delay a lone writer waits it out. Under load that left every write
// queued on the write lock alone: thousands of goroutines, and a node that
// fell further behind the longer it ran.

// maxGroup bounds the writes one transaction carries.
const maxGroup = 1000

type committer struct {
	db    *bbolt.DB
	calls chan commitCall
	// commits counts transactions, for tests.
	commits atomic.Int64
}

type commitCall struct {
	fn  func(*bbolt.Tx) error
	err chan error
}

//nolint:gochecknoglobals // One committer per database, started with its first write.
var committers sync.Map // *bbolt.DB → *committer

// commit runs fn in a write transaction of db, grouped with whatever other
// writes are waiting. fn may run more than once — merging is idempotent.
func commit(db *bbolt.DB, fn func(*bbolt.Tx) error) error {
	v, loaded := committers.LoadOrStore(db, &committer{db: db, calls: make(chan commitCall, maxGroup)})
	c := v.(*committer) //nolint:forcetypeassert // Only committers are stored.

	if !loaded {
		go c.run()
	}

	errc := make(chan error, 1)
	c.calls <- commitCall{fn: fn, err: errc}

	return <-errc
}

func (c *committer) run() {
	for first := range c.calls {
		group := []commitCall{first}

	drain:
		for len(group) < maxGroup {
			select {
			case next := <-c.calls:
				group = append(group, next)
			default:
				break drain
			}
		}

		c.apply(group)
	}
}

// apply commits group in one transaction. A write that fails is taken out
// and run alone, so its error is its own; the rest commit together.
func (c *committer) apply(group []commitCall) {
	for len(group) > 0 {
		failed := -1

		c.commits.Add(1)

		err := c.db.Update(func(tx *bbolt.Tx) error {
			for i, call := range group {
				if err := safely(call.fn, tx); err != nil {
					failed = i

					return err
				}
			}

			return nil
		})

		if failed < 0 {
			for _, call := range group {
				call.err <- err
			}

			return
		}

		bad := group[failed]
		bad.err <- c.db.Update(func(tx *bbolt.Tx) error { return safely(bad.fn, tx) })

		group = append(group[:failed], group[failed+1:]...)
	}
}

// safely runs fn, turning a panic into its error: the committer serves
// every write to the database and must not die with one.
func safely(fn func(*bbolt.Tx) error, tx *bbolt.Tx) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("write panicked: %v", r)
		}
	}()

	return fn(tx)
}
