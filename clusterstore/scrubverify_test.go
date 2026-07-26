package clusterstore

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// memVerification is an in-memory VerificationIndex: it answers from what has
// been recorded, flushed or not, which is what the real one must also do.
type memVerification struct {
	mu       sync.Mutex
	stamps   map[string]time.Time
	flushed  map[string]time.Time
	flushes  int
	failNext bool
}

func newMemVerification() *memVerification {
	return &memVerification{stamps: make(map[string]time.Time), flushed: make(map[string]time.Time)}
}

func (m *memVerification) LastVerified(bucket, key string) (time.Time, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	at, ok := m.stamps[objectRef(bucket, key)]

	return at, ok
}

func (m *memVerification) RecordVerified(bucket, key string, at time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.stamps[objectRef(bucket, key)] = at
}

func (m *memVerification) Flush() error {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.flushes++

	for k, v := range m.stamps {
		m.flushed[k] = v
	}

	return nil
}

func (m *memVerification) count() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	return len(m.stamps)
}

// TestScrubRecordsVerification checks a pass stamps what it checked, and
// flushes before it returns.
func TestScrubRecordsVerification(t *testing.T) {
	fc := newFakeCluster(3, 1)
	c := fc.coordinator(t, Config{})

	keys := []string{"a.txt", "b.txt", "c.txt"}
	for _, key := range keys {
		mustPut(t, c, key, randBytes(64))
	}

	c.Flush()

	verify := newMemVerification()

	r, err := NewRepairer(RepairerConfig{
		Coordinator:  c,
		Self:         fc.topo.Nodes[0].ID,
		Verification: verify,
	})
	require.NoError(t, err)

	rep, err := r.Scrub(t.Context())
	require.NoError(t, err)
	require.Positive(t, rep.Objects)

	assert.Equal(t, rep.Objects, verify.count(), "every object swept is stamped")
	assert.Positive(t, verify.flushes, "and the stamps are flushed before the pass ends")

	for _, key := range keys {
		if _, held := verify.LastVerified("b", key); held {
			return
		}
	}

	t.Fatal("none of the objects this node holds was stamped")
}

// TestScrubSkipsWhatThisPassAlreadySwept is what replaces the set of keys a
// pass used to keep in memory: two of a node's disks can hold the same object,
// and repairing it twice is waste.
func TestScrubSkipsWhatThisPassAlreadySwept(t *testing.T) {
	fc := newFakeCluster(3, 2)
	c := fc.coordinator(t, Config{})

	mustPut(t, c, "a.txt", randBytes(64))
	c.Flush()

	verify := newMemVerification()

	// Pretend the object was already swept by this pass, moments from now, so
	// the check cannot be satisfied by the timestamp merely being recent.
	verify.RecordVerified("b", "a.txt", time.Now().UTC().Add(time.Minute))

	r, err := NewRepairer(RepairerConfig{
		Coordinator:  c,
		Self:         fc.topo.Nodes[0].ID,
		Verification: verify,
	})
	require.NoError(t, err)

	rep, err := r.Scrub(t.Context())
	require.NoError(t, err)

	assert.Zero(t, rep.Objects, "an object this pass already swept is not swept again")
}

// TestScrubReVerifiesStaleStamps: a stamp from before this pass began is
// coverage from an earlier cycle, not a reason to skip. Treating it as one
// would mean an object verified once is never verified again.
func TestScrubReVerifiesStaleStamps(t *testing.T) {
	fc := newFakeCluster(3, 1)
	c := fc.coordinator(t, Config{})

	mustPut(t, c, "a.txt", randBytes(64))
	c.Flush()

	verify := newMemVerification()
	verify.RecordVerified("b", "a.txt", time.Now().UTC().Add(-24*time.Hour))

	r, err := NewRepairer(RepairerConfig{
		Coordinator:  c,
		Self:         fc.topo.Nodes[0].ID,
		Verification: verify,
	})
	require.NoError(t, err)

	rep, err := r.Scrub(t.Context())
	require.NoError(t, err)

	assert.Positive(t, rep.Objects, "yesterday's coverage does not excuse today's pass")
}

// TestScrubWithoutVerificationIndex keeps the old behavior available: a node
// without an index still dedups within a pass, out of memory.
func TestScrubWithoutVerificationIndex(t *testing.T) {
	fc := newFakeCluster(3, 2)
	c := fc.coordinator(t, Config{})

	for _, key := range []string{"a.txt", "b.txt"} {
		mustPut(t, c, key, randBytes(64))
	}

	c.Flush()

	r := newRepairer(t, c, fc.topo.Nodes[0].ID, false)

	first, err := r.Scrub(t.Context())
	require.NoError(t, err)

	second, err := r.Scrub(t.Context())
	require.NoError(t, err)

	assert.Equal(t, first.Objects, second.Objects, "each pass sweeps the same objects")
}

var _ VerificationIndex = (*memVerification)(nil)
