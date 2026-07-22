package handler_test

import (
	"fmt"
	"net/http"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestPutObject_IfNoneMatchConcurrentSingleWinner is the regression test for the
// conditional-PUT race: many goroutines race to create the same key with
// If-None-Match: *, and exactly one must win (200) while the rest get 412.
// A check-then-act implementation (stat, then write) lets several racers pass
// the existence check before any write lands, producing multiple winners.
func TestPutObject_IfNoneMatchConcurrentSingleWinner(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a", "", nil).Code)

	const racers = 32

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
		codes   = make(map[int]int)
	)

	start := make(chan struct{})

	for i := range racers {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			<-start

			rec := do(t, h, http.MethodPut, "/bucket-a/race", fmt.Sprintf("body-%d", i),
				map[string]string{"If-None-Match": "*"})

			mu.Lock()
			codes[rec.Code]++

			if rec.Code == http.StatusOK {
				winners++
			}
			mu.Unlock()
		}(i)
	}

	close(start)
	wg.Wait()

	require.Equal(t, 1, winners, "exactly one racer must win, got codes: %v", codes)
	require.Equal(t, racers-1, codes[http.StatusPreconditionFailed], "losers must all get 412")
}

// TestPutObject_IfMatchConcurrentSingleWinner is the compare-and-swap variant of
// the race: an object exists at ETag E0; many goroutines each PUT distinct
// content under If-Match: "E0". Exactly one must win (200) — its write moves the
// object off E0 — and every other must get 412. A check-then-act backend lets
// several observe E0 and all overwrite.
func TestPutObject_IfMatchConcurrentSingleWinner(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a", "", nil).Code)

	// Seed the object and capture its ETag (E0), which every racer targets.
	seed := do(t, h, http.MethodPut, "/bucket-a/cas", "v0", nil)
	require.Equal(t, http.StatusOK, seed.Code)

	e0 := seed.Header().Get("ETag")
	require.NotEmpty(t, e0)

	const racers = 32

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		winners int
		codes   = make(map[int]int)
	)

	start := make(chan struct{})

	for i := range racers {
		wg.Add(1)

		go func(i int) {
			defer wg.Done()

			<-start

			rec := do(t, h, http.MethodPut, "/bucket-a/cas", fmt.Sprintf("cas-body-%d", i),
				map[string]string{"If-Match": e0})

			mu.Lock()
			codes[rec.Code]++

			if rec.Code == http.StatusOK {
				winners++
			}
			mu.Unlock()
		}(i)
	}

	close(start)
	wg.Wait()

	require.Equal(t, 1, winners, "exactly one CAS racer must win, got codes: %v", codes)
	require.Equal(t, racers-1, codes[http.StatusPreconditionFailed], "losers must all get 412")
}
