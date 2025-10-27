package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/go-faster/fs"
)

func newS3Command() *cobra.Command {
	var (
		addr     string
		root     string
		logLevel string
	)

	cmd := &cobra.Command{
		Use:   "s3",
		Short: "Start S3-compatible storage server",
		Long: `Start an S3-compatible storage server.

This command starts a lightweight S3-compatible storage server that implements
basic S3 API operations including:
  - Bucket operations (create, delete, list)
  - Object operations (put, get, delete, list)

The server stores data in a local directory and provides an HTTP interface
compatible with S3 clients.`,
		Example: `  # Start server on default port (8080) with default data directory
  fs s3

  # Start server on custom port with custom data directory
  fs s3 --addr :9000 --root /data/s3

  # Start server and bind to specific interface
  fs s3 --addr 127.0.0.1:8080`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runS3Server(cmd.Context(), addr, root, logLevel)
		},
	}

	cmd.Flags().StringVar(&addr, "addr", ":8080", "Address to listen on")
	cmd.Flags().StringVar(&root, "root", ".s3data", "Root directory for S3 storage")
	cmd.Flags().StringVar(&logLevel, "log-level", "info", "Log level (debug, info, warn, error)")

	return cmd
}

func runS3Server(ctx context.Context, addr, root, logLevel string) error {
	// TODO: Use logLevel for configuring logging verbosity
	_ = logLevel

	// Make root path absolute
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return fmt.Errorf("failed to resolve root path: %w", err)
	}

	// Create S3 server
	s3Server, err := fs.NewS3Server(absRoot)
	if err != nil {
		return fmt.Errorf("failed to create S3 server: %w", err)
	}

	// Create HTTP server
	mux := http.NewServeMux()
	mux.Handle("/", s3Server)

	// Add health check endpoint
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		if _, err := fmt.Fprintf(w, "OK"); err != nil {
			// Log error but don't fail since headers already sent
			fmt.Fprintf(os.Stderr, "Health check write error: %v\n", err)
		}
	})

	server := &http.Server{
		Addr:         addr,
		Handler:      loggingMiddleware(mux),
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	// Setup graceful shutdown
	shutdownCh := make(chan struct{})
	go func() {
		sigint := make(chan os.Signal, 1)
		signal.Notify(sigint, os.Interrupt, syscall.SIGTERM)
		select {
		case <-sigint:
			fmt.Println("\nReceived interrupt signal, shutting down...")
		case <-ctx.Done():
			fmt.Println("\nContext canceled, shutting down...")
		}

		// Give outstanding requests 30 seconds to complete
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		if err := server.Shutdown(shutdownCtx); err != nil {
			fmt.Fprintf(os.Stderr, "HTTP server shutdown error: %v\n", err)
		}
		close(shutdownCh)
	}()

	// Start server
	fmt.Printf("S3-compatible server starting on %s\n", addr)
	fmt.Printf("Storage root: %s\n", absRoot)
	fmt.Printf("Health check available at http://%s/health\n", addr)
	fmt.Println("\nPress Ctrl+C to stop the server")

	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("HTTP server error: %w", err)
	}

	<-shutdownCh
	fmt.Println("Server stopped")
	return nil
}

// loggingMiddleware logs HTTP requests
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		// Create a response writer wrapper to capture status code
		ww := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		next.ServeHTTP(ww, r)

		duration := time.Since(start)
		fmt.Printf("[%s] %s %s - %d (%v)\n",
			start.Format("2006-01-02 15:04:05"),
			r.Method,
			r.URL.Path,
			ww.statusCode,
			duration,
		)
	})
}

// responseWriter wraps http.ResponseWriter to capture status code
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}
