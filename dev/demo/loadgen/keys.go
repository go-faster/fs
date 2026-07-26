package main

import (
	"bytes"
	crypto "crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// keyring remembers what has been written, so reads and deletes have something
// real to aim at.
//
// It is deliberately bounded and lossy: the generator runs for as long as the
// demo is up, and remembering every key it ever wrote would make the load
// generator the biggest consumer of memory in the compose file. Old keys age
// out of the sample rather than accumulating — a read that misses one is
// indistinguishable from a read of an object someone else deleted, which is
// itself realistic.
type keyring struct {
	mu   sync.Mutex
	keys map[string][]string
}

// keyringPerBucket is how many keys are remembered per bucket. Enough that
// reads hit, small enough that the memory is a rounding error.
const keyringPerBucket = 2048

// sample picks an index below n.
//
// Which remembered key to touch is a workload-shaping decision and protects
// nothing, so it uses the fast generator. The identifiers that must not collide
// come from crypto/rand instead.
func sample(n int) int {
	return rand.IntN(n) //nolint:gosec // Workload shaping, not security.
}

// sampleSize picks an object size in [min, max).
func sampleSize(minSize, maxSize int64) int64 {
	if maxSize <= minSize {
		return minSize
	}

	return minSize + rand.Int64N(maxSize-minSize) //nolint:gosec // Workload shaping, not security.
}

// sampleChance returns a uniform draw in [0, 1), for the operation mix.
func sampleChance() float64 {
	return rand.Float64() //nolint:gosec // Workload shaping, not security.
}

func newKeyring() *keyring {
	return &keyring{keys: make(map[string][]string)}
}

// add records a key, evicting a random older one once the sample is full.
func (k *keyring) add(bucket, key string) {
	k.mu.Lock()
	defer k.mu.Unlock()

	keys := k.keys[bucket]
	if len(keys) < keyringPerBucket {
		k.keys[bucket] = append(keys, key)

		return
	}

	keys[sample(len(keys))] = key
	k.keys[bucket] = keys
}

// pick returns a remembered key without removing it.
func (k *keyring) pick(bucket string) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()

	keys := k.keys[bucket]
	if len(keys) == 0 {
		return "", false
	}

	return keys[sample(len(keys))], true
}

// take removes and returns a remembered key, for a delete.
func (k *keyring) take(bucket string) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()

	keys := k.keys[bucket]
	if len(keys) == 0 {
		return "", false
	}

	i := sample(len(keys))
	key := keys[i]

	keys[i] = keys[len(keys)-1]
	k.keys[bucket] = keys[:len(keys)-1]

	return key, true
}

// forget drops a key that turned out not to be there.
func (k *keyring) forget(bucket, key string) {
	k.mu.Lock()
	defer k.mu.Unlock()

	keys := k.keys[bucket]

	for i, existing := range keys {
		if existing != key {
			continue
		}

		keys[i] = keys[len(keys)-1]
		k.keys[bucket] = keys[:len(keys)-1]

		return
	}
}

// newKey mints an object key under one of sixteen prefixes.
//
// The nesting is the point: a flat keyspace never exercises a delimiter
// listing, and a delimiter listing over a prefix holding thousands of keys is
// exactly what the index in front of it exists for.
func newKey(bucket string) string {
	var b [8]byte

	_, _ = crypto.Read(b[:])

	return fmt.Sprintf("d%d/%s/%s-%s",
		sample(16),
		time.Now().UTC().Format("2006-01-02"),
		bucket,
		hex.EncodeToString(b[:]),
	)
}

// payloadBlock is one buffer of random bytes that every object is cut from.
//
// Generated once: the alternative is a fresh stream per object, which costs
// nothing to produce but cannot be replayed — and a client that cannot replay a
// body cannot retry a request. That matters exactly when a node goes away
// mid-upload, which is the failure this demo exists to show.
var payloadBlock = func() []byte {
	// Larger than the biggest object, so any size can be cut from it.
	b := make([]byte, 64<<20)
	_, _ = crypto.Read(b)

	return b
}()

// newPayload returns a replayable reader of n incompressible bytes.
//
// Random rather than zeroes on purpose: zeroes compress to nothing, and a
// filesystem or transport that quietly did so would make every size in the
// dashboard a fiction. Seekable on purpose too, so an SDK retrying a request
// against another node can rewind it.
func newPayload(n int64) io.ReadSeeker {
	if n > int64(len(payloadBlock)) {
		n = int64(len(payloadBlock))
	}

	// A different window per object, so two objects of a size are not the same
	// bytes and their checksums differ.
	start := sample(len(payloadBlock) - int(n) + 1)

	return bytes.NewReader(payloadBlock[start : start+int(n)])
}

// discard drains a reader, returning how much it held.
func discard(r io.Reader) (int64, error) {
	return io.Copy(io.Discard, r)
}

// human renders a byte count the way the dashboard does.
func human(n int64) string {
	const unit = 1024

	if n < unit {
		return strconv.FormatInt(n, 10) + " B"
	}

	div, exp := int64(unit), 0

	for n/div >= unit && exp < 4 {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTP"[exp])
}

// env reads a setting, falling back to a default.
func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}

	return fallback
}

func envInt(key string, fallback int) int {
	v, err := strconv.Atoi(env(key, ""))
	if err != nil || v <= 0 {
		return fallback
	}

	return v
}

func envFloat(key string, fallback float64) float64 {
	v, err := strconv.ParseFloat(env(key, ""), 64)
	if err != nil || v <= 0 {
		return fallback
	}

	return v
}

// splitList parses a comma-separated setting.
func splitList(s string) []string {
	var out []string

	for _, part := range strings.Split(s, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}

	return out
}
