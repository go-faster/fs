package server_test

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"time"

	"github.com/go-faster/fs/server"
	"github.com/go-faster/fs/storagemem"
)

// ExampleNewHandler demonstrates the low-level API: build a bare http.Handler
// for a storage backend and mount it into your own server or mux.
func ExampleNewHandler() {
	store := storagemem.New()

	h := server.NewHandler(store)

	// Mount under a prefix in your own mux.
	mux := http.NewServeMux()
	mux.Handle("/s3/", http.StripPrefix("/s3", h))

	ts := httptest.NewServer(mux)
	defer ts.Close()

	// Create a bucket via the embedded S3 API.
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/s3/example-bucket", http.NoBody)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatal(err)
	}

	defer func() { _ = resp.Body.Close() }()

	fmt.Println(resp.StatusCode)
	// Output: 200
}

// ExampleServer demonstrates the high-level API: a turnkey server with a health
// endpoint, timeouts and graceful shutdown driven by a context.
func ExampleServer() {
	store := storagemem.New()

	srv, err := server.New(server.Config{
		Storage:    store,
		Addr:       "127.0.0.1:0", // ephemeral port
		Buckets:    []string{"example-bucket"},
		HealthPath: "/health",
		// WrapHandler is the injection point for observability or middleware,
		// e.g. otelhttp.NewHandler(h, "s3") or a request logger.
	})
	if err != nil {
		log.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx) }()

	// ... serve requests ...
	time.Sleep(10 * time.Millisecond)

	cancel() // trigger graceful shutdown

	if err := <-done; err != nil {
		log.Fatal(err)
	}

	fmt.Println("stopped cleanly")
	// Output: stopped cleanly
}
