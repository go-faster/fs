package table

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/bbolt"
)

func testDB(t *testing.T) *bbolt.DB {
	t.Helper()

	db, err := OpenDB(filepath.Join(t.TempDir(), "meta.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	db.NoSync = true

	require.NoError(t, db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.CreateBucket([]byte("b"))

		return err
	}))

	return db
}

// TestCommitGroups: writes that arrive together share transactions, and
// every one of them lands.
func TestCommitGroups(t *testing.T) {
	db := testDB(t)

	var wg sync.WaitGroup

	for i := range 2000 {
		wg.Go(func() {
			assert.NoError(t, commit(db, func(tx *bbolt.Tx) error {
				return tx.Bucket([]byte("b")).Put(fmt.Appendf(nil, "k%d", i), []byte("v"))
			}))
		})
	}

	wg.Wait()

	n := 0

	require.NoError(t, db.View(func(tx *bbolt.Tx) error {
		n = tx.Bucket([]byte("b")).Stats().KeyN

		return nil
	}))
	assert.Equal(t, 2000, n)

	v, _ := committers.Load(db)
	commits := v.(*committer).commits.Load()
	assert.Less(t, commits, int64(1000), "2000 concurrent writes share transactions (%d commits)", commits)
	t.Logf("2000 writes in %d commits", commits)
}

// TestCommitIsolatesFailure: a failing write gets its own error, and the
// writes grouped with it still commit.
func TestCommitIsolatesFailure(t *testing.T) {
	db := testDB(t)
	boom := errors.New("boom")

	var wg sync.WaitGroup

	errs := make([]error, 50)

	for i := range 50 {
		wg.Go(func() {
			errs[i] = commit(db, func(tx *bbolt.Tx) error {
				if i == 25 {
					return boom
				}

				if i == 30 {
					panic("bad write")
				}

				return tx.Bucket([]byte("b")).Put(fmt.Appendf(nil, "k%d", i), []byte("v"))
			})
		})
	}

	wg.Wait()

	for i, err := range errs {
		switch i {
		case 25:
			require.ErrorIs(t, err, boom)
		case 30:
			require.ErrorContains(t, err, "panicked")
		default:
			require.NoError(t, err, "write %d", i)
		}
	}

	require.NoError(t, db.View(func(tx *bbolt.Tx) error {
		assert.Equal(t, 48, tx.Bucket([]byte("b")).Stats().KeyN)

		return nil
	}))
}
