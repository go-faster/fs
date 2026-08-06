package diskstore_test

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/diskstore"
)

// fragmentName is a fragment path in the layout the coordinator mints:
// obj/<64 hex>/<64 hex>/<name>.
func fragmentName(object, generation int) string {
	return fmt.Sprintf("obj/%064x/%064x/%016x.f1", object, generation, object)
}

// TestCreateSurvivesAConcurrentPrune is the race itself, and the reason #170
// showed up as a different test every time: any PUT can be the one whose
// MkdirAll is interrupted, so the failure lands wherever the suite happens to
// be.
//
// Writes and deletes of *different* objects, which is what the conformance
// suite does continuously. Nothing here should ever fail: a create racing a
// prune is not a caller error, and returning one turns a healthy cluster's
// ordinary teardown into a 500.
func TestCreateSurvivesAConcurrentPrune(t *testing.T) {
	dir := t.TempDir()

	s, err := diskstore.New(map[cluster.DiskID]string{"d0": dir})
	require.NoError(t, err)

	const (
		workers = 8
		rounds  = 150
	)

	var (
		wg      sync.WaitGroup
		failed  atomic.Int64
		example atomic.Value
	)

	for w := range workers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for i := range rounds {
				name := fragmentName(w*rounds+i, 1)

				f, err := s.Create(t.Context(), "d0", name)
				if err != nil {
					failed.Add(1)
					example.Store(err.Error())

					continue
				}

				_, _ = f.Write([]byte("fragment"))
				_ = f.Close()

				// Deleting it empties the object's directories, and the prune
				// walks up from there — into the parents the other workers are
				// creating into right now.
				_ = s.Delete(t.Context(), "d0", name)
			}
		}()
	}

	wg.Wait()

	if got := failed.Load(); got > 0 {
		msg, _ := example.Load().(string)
		t.Fatalf("%d creates failed while other objects were being deleted; one was: %s", got, msg)
	}
}

// TestCreateSurvivesAPruneOfItsOwnObject is the half of the race that keeping
// the fragment root does not remove.
//
// Generations of one object share a directory, so a delete of an old generation
// prunes exactly the parent a write of a new one is creating into. Keeping the
// namespace root is no help a level down; only the create's retry is.
func TestCreateSurvivesAPruneOfItsOwnObject(t *testing.T) {
	dir := t.TempDir()

	s, err := diskstore.New(map[cluster.DiskID]string{"d0": dir})
	require.NoError(t, err)

	const (
		workers = 8
		rounds  = 150
	)

	var (
		wg      sync.WaitGroup
		failed  atomic.Int64
		example atomic.Value
	)

	for w := range workers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for i := range rounds {
				// One object, many generations: every worker's delete prunes
				// the directory every other worker's create needs.
				name := fragmentName(1, w*rounds+i)

				f, err := s.Create(t.Context(), "d0", name)
				if err != nil {
					failed.Add(1)
					example.Store(err.Error())

					continue
				}

				_, _ = f.Write([]byte("fragment"))
				_ = f.Close()
				_ = s.Delete(t.Context(), "d0", name)
			}
		}()
	}

	wg.Wait()

	if got := failed.Load(); got > 0 {
		msg, _ := example.Load().(string)
		t.Fatalf("%d creates failed while other generations were being deleted; one was: %s", got, msg)
	}
}
