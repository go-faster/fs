package bench

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/go-faster/fs"
)

// These gates encode DESIGN.md NFR-3. The allocation gate is deterministic and
// platform-independent, so it runs in every `go test ./...`. The throughput
// and latency gates measure wall-clock behavior, which is only meaningful on a
// consistent reference environment — the general correctness matrix spans slow
// shared macOS/windows/386 runners where absolute latency is pure noise and
// moving hundreds of MiB is wasteful. They therefore run only when
// FS_PERF_GATES is set, which the perf workflow does; elsewhere they
// skip. The throughput gate stays a machine-relative ratio even so.

// nfr3ThroughputRatio is the NFR-3 large-object floor: backend throughput must
// be at least this fraction of the disk's raw sequential bandwidth.
const nfr3ThroughputRatio = 0.80

// requirePerfGates skips a wall-clock gate unless FS_PERF_GATES is set (the
// perf workflow sets it). Keeps timing gates off the noisy multi-platform
// correctness matrix.
func requirePerfGates(t *testing.T) {
	t.Helper()

	if os.Getenv("FS_PERF_GATES") == "" {
		t.Skip("wall-clock gate; set FS_PERF_GATES=1 (perf.yml) to run — it needs a consistent environment")
	}
}

// TestNFR3PutAllocsPerBlock verifies a PUT streams: whatever it allocates
// beyond a fixed per-request overhead is a constant per block, never a
// function of what the body has accumulated so far.
//
// The engine stores an object as blocks, and each block is its own reference
// row and block write, so allocations grow with the block count. What must not
// happen is anything worse than linear in blocks — buffering, or work that
// grows with the object.
func TestNFR3PutAllocsPerBlock(t *testing.T) {
	if testing.Short() {
		t.Skip("allocation gate builds large objects")
	}

	s, _ := benchStore(t)

	measure := func(size int64) (allocs float64) {
		body := newBody(size)
		i := 0

		return testing.AllocsPerRun(3, func() {
			putObject(t, s, fmt.Sprintf("a-%d-%d", size, i), size, body)
			i++
		})
	}

	small := measure(64 << 10)
	large := measure(sizeHuge)

	const (
		blockSize      = 1 << 20 // engine.DefaultBlockSize.
		perBlockBudget = 128
	)

	blocks := float64(sizeHuge / blockSize)
	perBlock := (large - small) / blocks

	t.Logf("PUT allocs/op: 64KiB=%.0f, 256MiB=%.0f (%.0f blocks, %.1f allocs/block)", small, large, blocks, perBlock)

	assert.LessOrEqual(t, perBlock, float64(perBlockBudget),
		"PUT allocations must stay a constant per block; got %.1f per block", perBlock)
}

// TestNFR3PutMemoryBounded is the guarantee the allocation gate stands in for:
// a PUT holds a few blocks at once, not the object. The live heap is sampled
// while a 256 MiB object is written, under an aggressive GC so that garbage
// does not read as live, and must stay far below the object's size.
func TestNFR3PutMemoryBounded(t *testing.T) {
	if testing.Short() {
		t.Skip("memory gate builds a large object")
	}

	s, _ := benchStore(t)

	defer debug.SetGCPercent(debug.SetGCPercent(10))

	sample := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}

	runtime.GC()
	metrics.Read(sample)
	base := sample[0].Value.Uint64()

	var (
		peak atomic.Uint64
		done = make(chan struct{})
		wg   sync.WaitGroup
	)

	wg.Go(func() {
		local := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}}

		for {
			select {
			case <-done:
				return
			case <-time.After(time.Millisecond):
			}

			metrics.Read(local)

			if v := local[0].Value.Uint64(); v > peak.Load() {
				peak.Store(v)
			}
		}
	})

	putObject(t, s, "memory", sizeHuge, newBody(sizeHuge))
	close(done)
	wg.Wait()

	const ceiling = 64 << 20

	grew := int64(peak.Load()) - int64(base) //nolint:gosec // Heap sizes fit in int64.
	t.Logf("PUT of %d MiB: live heap grew by at most %d MiB", sizeHuge>>20, grew>>20)

	assert.Less(t, grew, int64(ceiling), "a PUT must hold a few blocks, not the object")
}

// TestNFR3LargeObjectThroughput gates large-object PUT and GET throughput at
// ≥80% of the same disk's raw sequential bandwidth.
func TestNFR3LargeObjectThroughput(t *testing.T) {
	requirePerfGates(t)

	if testing.Short() {
		t.Skip("throughput gate moves hundreds of MiB")
	}

	if raceEnabled {
		t.Skip("race instrumentation distorts the throughput ratio; run without -race (perf.yml)")
	}

	s, dir := benchStore(t)

	// Raw baselines on the same filesystem. rawWrite is pure disk (reference);
	// rawWriteHash streams through MD5 too — the true ceiling for a compliant
	// PUT, whose S3 ETag mandates hashing every byte (MD5 is slower than NVMe
	// sequential write, so it, not the disk, bounds large-object PUT).
	rawWrite := rawSequentialWrite(t, dir, sizeLarge, 4*sizeLarge, false)
	rawWriteHash := rawSequentialWrite(t, dir, sizeLarge, 4*sizeLarge, true)
	rawRead := rawSequentialRead(t, dir, sizeLarge)

	// Backend PUT: several large objects, aggregate rate.
	const puts = 4

	body := newBody(sizeLarge)

	startW := time.Now()

	for i := range puts {
		putObject(t, s, fmt.Sprintf("thr-%d", i), sizeLarge, body)
	}

	putRate := float64(puts*sizeLarge) / time.Since(startW).Seconds()

	// Backend GET of one of them (page-cache warm, like the raw read baseline).
	startR := time.Now()
	got := getObjectDiscard(t, s, "thr-0")
	getRate := float64(got) / time.Since(startR).Seconds()

	t.Logf("PUT: %s — %.0f%% of stream+MD5+write ceiling %s (pure disk %s, MD5-bound)",
		mbps(putRate), 100*putRate/rawWriteHash, mbps(rawWriteHash), mbps(rawWrite))
	t.Logf("GET: %s — %.0f%% of raw read %s", mbps(getRate), 100*getRate/rawRead, mbps(rawRead))

	// PUT is gated against the hash-inclusive ceiling: the backend must add
	// little over the mandatory stream-hash-write work (temp file + rename +
	// sidecar), not against pure disk bandwidth it cannot reach with MD5 on
	// the critical path.
	assert.GreaterOrEqual(t, putRate/rawWriteHash, nfr3ThroughputRatio,
		"PUT throughput %s below %.0f%% of the stream+MD5+write ceiling %s", mbps(putRate), 100*nfr3ThroughputRatio, mbps(rawWriteHash))
	assert.GreaterOrEqual(t, getRate/rawRead, nfr3ThroughputRatio,
		"GET throughput %s below %.0f%% of raw read %s", mbps(getRate), 100*nfr3ThroughputRatio, mbps(rawRead))
}

// TestNFR3SmallObjectGetLatency measures 4 KiB GET p99 under concurrent load.
// The NFR-3 target is p99 < 10 ms at 5k req/s on an NVMe node; CI runs on
// slower shared hardware, so the gate is a generous ceiling and the real p99
// is logged for regression tracking.
func TestNFR3SmallObjectGetLatency(t *testing.T) {
	requirePerfGates(t)

	if testing.Short() {
		t.Skip("latency gate runs a concurrent load phase")
	}

	if raceEnabled {
		t.Skip("race instrumentation inflates latencies; run without -race (perf.yml)")
	}

	s, _ := benchStore(t)

	// Populate a working set.
	const keys = 512

	body := newBody(sizeSmall)
	for i := range keys {
		putObject(t, s, fmt.Sprintf("k-%d", i), sizeSmall, body)
	}

	const (
		workers = 16
		perW    = 400
		// Generous ceiling: the gate catches gross regressions (the NVMe
		// target is p99 < 10 ms; a healthy backend sits far under this even on
		// a shared perf runner), not a tight SLA. The real p99 is logged.
		ceilCIp99 = 100 * time.Millisecond
	)

	lat := make([][]time.Duration, workers)

	var wg sync.WaitGroup

	start := time.Now()

	for w := range workers {
		wg.Go(func() {
			samples := make([]time.Duration, 0, perW)
			ctx := context.Background()

			for i := range perW {
				key := fmt.Sprintf("k-%d", (w*perW+i)%keys)

				t0 := time.Now()

				resp, err := s.GetObject(ctx, "bench", key)
				if err == nil {
					_ = drain(resp)
				}

				samples = append(samples, time.Since(t0))
			}

			lat[w] = samples
		})
	}

	wg.Wait()

	elapsed := time.Since(start)

	var all []time.Duration
	for _, s := range lat {
		all = append(all, s...)
	}

	p50, p99, p999 := percentile(all, 0.50), percentile(all, 0.99), percentile(all, 0.999)
	rate := float64(len(all)) / elapsed.Seconds()

	t.Logf("4KiB GET under %d workers: %.0f req/s, p50=%s p99=%s p99.9=%s (NFR-3 target: p99<10ms @ 5k req/s on NVMe)",
		workers, rate, p50.Round(time.Microsecond), p99.Round(time.Microsecond), p999.Round(time.Microsecond))

	assert.Less(t, p99, ceilCIp99, "4KiB GET p99 %s exceeds the CI ceiling", p99)
}

// drain reads and closes a GET response body.
func drain(resp *fs.GetObjectResponse) int64 {
	var n int64

	buf := make([]byte, 8<<10)

	for {
		m, err := resp.Reader.Read(buf)
		n += int64(m)

		if err != nil {
			_ = resp.Reader.Close()
			return n
		}
	}
}

// percentile returns the p-quantile (0..1) of durations.
func percentile(d []time.Duration, p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}

	sorted := append([]time.Duration(nil), d...)
	slices.Sort(sorted)

	idx := int(p * float64(len(sorted)-1))

	return sorted[idx]
}

// mbps renders a bytes/sec rate.
func mbps(bytesPerSec float64) string {
	return fmt.Sprintf("%.0f MB/s", bytesPerSec/1e6)
}
