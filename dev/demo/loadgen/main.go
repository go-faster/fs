// Command loadgen keeps the demo cluster busy with a workload shaped like a
// real one.
//
// The point is not throughput — it is that the dashboard has something honest
// to show. Object stores are not exercised by one bucket of identically sized
// blobs: they are exercised by a handful of buckets with different shapes, a
// size distribution spanning three orders of magnitude, and a read-heavy mix
// with listings and deletes mixed in. That is what this produces, at a rate low
// enough to leave a laptop usable.
package main

import (
	"context"
	"fmt"
	"log"
	"os/signal"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// bucketSpec is one bucket and the object sizes it holds.
//
// Real deployments do not have one size distribution; they have a few buckets
// with quite different ones, and the difference is what makes placement,
// multipart and listing behave differently from each other. So: thumbnails are
// tiny and numerous, media is large enough to go through multipart, logs sit in
// between and are written far more than they are read.
type bucketSpec struct {
	name string
	// min and max bound the object size, in bytes.
	min, max int64
	// weight is how much of the traffic lands here.
	weight int
}

var buckets = []bucketSpec{
	{name: "thumbnails", min: 4 << 10, max: 64 << 10, weight: 40},
	{name: "documents", min: 64 << 10, max: 2 << 20, weight: 30},
	{name: "logs", min: 16 << 10, max: 512 << 10, weight: 20},
	{name: "media", min: 8 << 20, max: 48 << 20, weight: 10},
}

// A worker either writes or reads, and never both.
//
// Mixing them in one worker looks more natural and behaves worse: a write to a
// cluster that is refusing them occupies its worker for as long as the body
// takes to stream and be rejected, and with every worker doing both, all of
// them end up parked in writes. The reads that would have succeeded are then
// never issued, and a cluster that is serving reads perfectly well looks
// completely dead. Separating them is also what a real deployment looks like —
// the thing uploading is rarely the thing browsing.
type role int

const (
	roleWriter role = iota
	roleReader
)

// stats counts what happened, for the line printed every few seconds.
type stats struct {
	puts, gets, lists, deletes, errors atomic.Int64
	bytesWritten, bytesRead            atomic.Int64
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	endpoints := splitList(env("FS_ENDPOINTS", "127.0.0.1:9001"))
	if len(endpoints) == 0 {
		return fmt.Errorf("FS_ENDPOINTS is empty")
	}

	rate := envFloat("FS_RATE", 10)
	workers := envInt("FS_WORKERS", 4)

	clients := make([]*minio.Client, 0, len(endpoints))

	for _, endpoint := range endpoints {
		client, err := minio.New(endpoint, &minio.Options{
			Creds:  credentials.NewStaticV4(env("FS_ACCESS_KEY", "demo"), env("FS_SECRET_KEY", "demodemodemo"), ""),
			Secure: false,
		})
		if err != nil {
			return fmt.Errorf("client for %s: %w", endpoint, err)
		}

		clients = append(clients, client)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Every node serves every key, so the buckets need creating once — but a
	// cluster that is still converging refuses writes, and this starts the
	// moment the health check passes. Retrying is the difference between a demo
	// that comes up and one that needs a second `up`.
	if err := ensureBuckets(ctx, clients[0]); err != nil {
		return err
	}

	// A refused write is an answer, not a hiccup: retrying it ten times only
	// keeps a worker busy while the cluster is telling it something true.
	minio.MaxRetry = 2

	log.Printf("load: %d workers, %.0f ops/s across %d endpoints", workers, rate, len(clients))

	var (
		st   stats
		keys = newKeyring()
		wg   sync.WaitGroup
	)

	// One token bucket shared by every worker, so the configured rate is the
	// rate of the whole generator rather than of each worker.
	tick := time.Duration(float64(time.Second) / rate)
	if tick <= 0 {
		tick = time.Millisecond
	}

	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	go report(ctx, &st)

	// Half write, half read, with at least one of each however few there are.
	for i := range workers {
		r := roleWriter
		if i%2 == 1 || workers == 1 {
			r = roleReader
		}

		wg.Go(func() {
			worker(ctx, clients[i%len(clients)], r, ticker.C, &st, keys)
		})
	}

	wg.Wait()
	log.Print("load: stopped")

	return nil
}

// ensureBuckets creates the workload's buckets, waiting for the cluster to be
// able to hold them.
func ensureBuckets(ctx context.Context, client *minio.Client) error {
	deadline := time.Now().Add(2 * time.Minute)

	for _, spec := range buckets {
		for {
			err := client.MakeBucket(ctx, spec.name, minio.MakeBucketOptions{})
			if err == nil {
				break
			}

			if exists, checkErr := client.BucketExists(ctx, spec.name); checkErr == nil && exists {
				break
			}

			if ctx.Err() != nil {
				return ctx.Err()
			}

			if time.Now().After(deadline) {
				return fmt.Errorf("create bucket %s: %w", spec.name, err)
			}

			log.Printf("waiting for the cluster to accept %s: %v", spec.name, err)
			time.Sleep(2 * time.Second)
		}
	}

	return nil
}

// opTimeout bounds one operation.
//
// It exists because of what a node loss does to a workload without it. Writes
// are legitimately refused while a three-node cluster is down to two — rf2.5
// has nowhere safe to put the third fragment — and an SDK retries a refused
// request several times with backoff. Four workers then spend the whole outage
// inside retries of writes that cannot succeed, and the reads that would have
// worked never get issued: the cluster looks entirely dead when only half of it
// is.
const opTimeout = 30 * time.Second

// worker runs operations until the context ends, one per tick.
func worker(ctx context.Context, client *minio.Client, r role, tick <-chan time.Time, st *stats, keys *keyring) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick:
		}

		opCtx, cancel := context.WithTimeout(ctx, opTimeout)
		err := operate(opCtx, client, r, st, keys)

		cancel()

		if err != nil && ctx.Err() == nil {
			st.errors.Add(1)

			log.Printf("op failed: %v", err)
		}
	}
}

// operate performs one operation for a worker of this role, with listings and
// deletes threaded through — the shape traffic actually has.
func operate(ctx context.Context, client *minio.Client, r role, st *stats, keys *keyring) error {
	spec := pickBucket()
	roll := sampleChance()

	if r == roleReader {
		if roll < 0.15 {
			return list(ctx, client, spec, st)
		}

		return get(ctx, client, spec, st, keys)
	}

	if roll < 0.15 {
		return remove(ctx, client, spec, st, keys)
	}

	return put(ctx, client, spec, st, keys)
}

// pickBucket chooses a bucket by weight.
func pickBucket() bucketSpec {
	total := 0
	for _, spec := range buckets {
		total += spec.weight
	}

	roll := sample(total)

	for _, spec := range buckets {
		if roll < spec.weight {
			return spec
		}

		roll -= spec.weight
	}

	return buckets[0]
}

func put(ctx context.Context, client *minio.Client, spec bucketSpec, st *stats, keys *keyring) error {
	size := sampleSize(spec.min, spec.max)

	key := newKey(spec.name)

	// Keys are nested so listings have prefixes to fold, which is what a
	// delimiter listing in the dashboard actually exercises.
	_, err := client.PutObject(ctx, spec.name, key, newPayload(size), size, minio.PutObjectOptions{
		ContentType: "application/octet-stream",
	})
	if err != nil {
		return fmt.Errorf("put %s/%s: %w", spec.name, key, err)
	}

	st.puts.Add(1)
	st.bytesWritten.Add(size)
	keys.add(spec.name, key)

	return nil
}

func get(ctx context.Context, client *minio.Client, spec bucketSpec, st *stats, keys *keyring) error {
	key, ok := keys.pick(spec.name)
	if !ok {
		// Nothing written to this bucket yet. A reader does not write instead:
		// that would put it back in the way of the writes it is meant to be
		// independent of.
		return nil
	}

	obj, err := client.GetObject(ctx, spec.name, key, minio.GetObjectOptions{})
	if err != nil {
		return fmt.Errorf("get %s/%s: %w", spec.name, key, err)
	}

	defer func() { _ = obj.Close() }()

	n, err := discard(obj)
	if err != nil {
		// A key this generator deleted, or one whose write failed: not worth
		// counting as an error against the cluster.
		keys.forget(spec.name, key)

		return nil //nolint:nilerr // A vanished key is expected churn.
	}

	st.gets.Add(1)
	st.bytesRead.Add(n)

	return nil
}

func list(ctx context.Context, client *minio.Client, spec bucketSpec, st *stats) error {
	var seen int

	for obj := range client.ListObjects(ctx, spec.name, minio.ListObjectsOptions{
		Prefix:    "d" + strconv.Itoa(sample(16)) + "/",
		Recursive: sample(2) == 0,
		MaxKeys:   200,
	}) {
		if obj.Err != nil {
			return fmt.Errorf("list %s: %w", spec.name, obj.Err)
		}

		seen++

		if seen >= 200 {
			break
		}
	}

	st.lists.Add(1)

	return nil
}

func remove(ctx context.Context, client *minio.Client, spec bucketSpec, st *stats, keys *keyring) error {
	key, ok := keys.take(spec.name)
	if !ok {
		return nil
	}

	if err := client.RemoveObject(ctx, spec.name, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("delete %s/%s: %w", spec.name, key, err)
	}

	st.deletes.Add(1)

	return nil
}

// report prints what has happened since the last line.
func report(ctx context.Context, st *stats) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	var last stats

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		puts := st.puts.Load()
		gets := st.gets.Load()
		lists := st.lists.Load()
		deletes := st.deletes.Load()
		errs := st.errors.Load()
		written := st.bytesWritten.Load()
		read := st.bytesRead.Load()

		log.Printf("put %d (+%d, %s) get %d (+%d, %s) list %d (+%d) delete %d (+%d) errors %d (+%d)",
			puts, puts-last.puts.Load(), human(written),
			gets, gets-last.gets.Load(), human(read),
			lists, lists-last.lists.Load(),
			deletes, deletes-last.deletes.Load(),
			errs, errs-last.errors.Load(),
		)

		last.puts.Store(puts)
		last.gets.Store(gets)
		last.lists.Store(lists)
		last.deletes.Store(deletes)
		last.errors.Store(errs)
	}
}
