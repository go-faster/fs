package diskstore_test

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/diskstore"
)

// recordObserver captures what the store reports.
type recordObserver struct {
	mu        sync.Mutex
	committed []string
	deleted   []string
	contents  map[string]string
}

func newRecordObserver() *recordObserver {
	return &recordObserver{contents: make(map[string]string)}
}

func (o *recordObserver) Wants(name string) bool { return strings.HasSuffix(name, "/meta") }

func (o *recordObserver) Committed(_ cluster.DiskID, name string, data []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.committed = append(o.committed, name)
	o.contents[name] = string(data)
}

func (o *recordObserver) Deleted(_ cluster.DiskID, name string, data []byte) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.deleted = append(o.deleted, name)
	o.contents["deleted:"+name] = string(data)
}

func (o *recordObserver) snapshot() (committed, deleted []string, contents map[string]string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	return append([]string(nil), o.committed...), append([]string(nil), o.deleted...),
		copyOf(o.contents)
}

func copyOf(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}

	return out
}

// TestObserverSeesCommitsAndDeletes covers the seam a node's object index
// hangs off: every record that lands on or leaves a disk is reported, with its
// content, and payload fragments are not — they carry no object identity and
// reading them back would be the whole disk.
func TestObserverSeesCommitsAndDeletes(t *testing.T) {
	obs := newRecordObserver()

	s, err := diskstore.New(
		map[cluster.DiskID]string{"d0": filepath.Join(t.TempDir(), "d0")},
		diskstore.WithObserver(obs),
	)
	require.NoError(t, err)

	put(t, s, "d0", "obj/aa/bb/meta", []byte(`{"bucket":"photos","key":"a.jpg"}`))
	put(t, s, "d0", "obj/aa/bb/gen1.f0", []byte("payload"))

	committed, deleted, contents := obs.snapshot()
	assert.Equal(t, []string{"obj/aa/bb/meta"}, committed, "only commit records are reported")
	assert.Empty(t, deleted)
	assert.JSONEq(t, `{"bucket":"photos","key":"a.jpg"}`, contents["obj/aa/bb/meta"])

	// An overwrite reports the new content, which is how the index learns a
	// new size.
	put(t, s, "d0", "obj/aa/bb/meta", []byte(`{"bucket":"photos","key":"a.jpg","size":9}`))

	_, _, contents = obs.snapshot()
	assert.JSONEq(t, `{"bucket":"photos","key":"a.jpg","size":9}`, contents["obj/aa/bb/meta"])

	// The delete carries the record's content: the name is a hash, so nothing
	// downstream could work out which object it was afterward.
	require.NoError(t, s.Delete(t.Context(), "d0", "obj/aa/bb/meta"))
	require.NoError(t, s.Delete(t.Context(), "d0", "obj/aa/bb/gen1.f0"))

	_, deleted, contents = obs.snapshot()
	assert.Equal(t, []string{"obj/aa/bb/meta"}, deleted)
	assert.JSONEq(t, `{"bucket":"photos","key":"a.jpg","size":9}`,
		contents["deleted:obj/aa/bb/meta"])
}

// TestObserverIsOptional: the store works without one, which is what every
// test cluster and any store built before the index does.
func TestObserverIsOptional(t *testing.T) {
	s := newStore(t, "d0")

	put(t, s, "d0", "obj/aa/bb/meta", []byte("{}"))
	require.NoError(t, s.Delete(t.Context(), "d0", "obj/aa/bb/meta"))
}
