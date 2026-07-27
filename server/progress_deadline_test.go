package server_test

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/server"
	"github.com/go-faster/fs/storagemem"
)

// startServer runs a real server on a loopback listener with the given
// timeouts. The deadlines under test only exist on a real connection, so
// httptest.NewRecorder cannot exercise them.
func startServer(t *testing.T, readTimeout, writeTimeout time.Duration) string {
	t.Helper()

	srv, err := server.New(server.Config{
		Storage:      storagemem.New(),
		ReadTimeout:  readTimeout,
		WriteTimeout: writeTimeout,
	})
	require.NoError(t, err)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	hs := srv.HTTPServer()
	go func() { _ = hs.Serve(ln) }()

	t.Cleanup(func() { _ = hs.Close() })

	return "http://" + ln.Addr().String()
}

func putObject(t *testing.T, base, bucket, key string, body []byte) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPut, base+"/"+bucket, http.NoBody)
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	_ = resp.Body.Close()

	req, err = http.NewRequest(http.MethodPut, base+"/"+bucket+"/"+key, bytes.NewReader(body))
	require.NoError(t, err)

	req.ContentLength = int64(len(body))
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)

	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
}

// TestServer_SlowClientGetsWholeObject is the regression test for a GET
// truncated by the write timeout.
//
// The timeouts http.Server offers cover a whole transfer, so serving an object
// took longer than WriteTimeout the moment the object was large or the client
// slow — and the client got a body short of the Content-Length it was
// promised, with nothing but a closed connection to say so. Here the client
// takes several times the timeout to read, and must still receive every byte.
func TestServer_SlowClientGetsWholeObject(t *testing.T) {
	const (
		timeout = 300 * time.Millisecond
		size    = 8 << 20
	)

	base := startServer(t, timeout, timeout)

	body := bytes.Repeat([]byte("abcdefgh"), size/8)
	putObject(t, base, "bucket", "big", body)

	resp, err := http.Get(base + "/bucket/big") //nolint:noctx // Deadlines are the subject; a context would mask them.
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, int64(size), resp.ContentLength)

	// Read at a pace that makes the whole transfer outlast the timeout many
	// times over, while never pausing long enough to look stalled.
	var (
		got   int
		chunk = make([]byte, 64<<10)
		start = time.Now()
	)

	for {
		n, err := resp.Body.Read(chunk)
		got += n

		if err == io.EOF {
			break
		}

		require.NoError(t, err, "read failed after %d of %d bytes", got, size)

		time.Sleep(10 * time.Millisecond)
	}

	require.Equal(t, size, got, "body truncated")
	require.Greater(t, time.Since(start), timeout,
		"read finished within the timeout, so it never exercised the deadline")
}

// TestServer_SlowClientCanUploadWholeObject is the same regression on the read
// side: a request body that takes longer than ReadTimeout to arrive must not
// be cut off while it is still making progress.
func TestServer_SlowClientCanUploadWholeObject(t *testing.T) {
	const (
		timeout = 300 * time.Millisecond
		chunks  = 40
		chunk   = 64 << 10
		size    = chunks * chunk
	)

	base := startServer(t, timeout, timeout)

	req, err := http.NewRequest(http.MethodPut, base+"/bucket", http.NoBody) //nolint:noctx // See above.
	require.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	_ = resp.Body.Close()

	pr, pw := io.Pipe()

	go func() {
		defer func() { _ = pw.Close() }()

		for range chunks {
			if _, err := pw.Write(bytes.Repeat([]byte("x"), chunk)); err != nil {
				return
			}

			time.Sleep(20 * time.Millisecond)
		}
	}()

	req, err = http.NewRequest(http.MethodPut, base+"/bucket/slow", pr) //nolint:noctx // See above.
	require.NoError(t, err)

	req.ContentLength = size

	start := time.Now()
	resp, err = http.DefaultClient.Do(req)
	require.NoError(t, err)

	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Greater(t, time.Since(start), timeout,
		"upload finished within the timeout, so it never exercised the deadline")

	head, err := http.Head(base + "/bucket/slow") //nolint:noctx // See above.
	require.NoError(t, err)

	_ = head.Body.Close()
	require.Equal(t, int64(size), head.ContentLength, "stored object is short")
}

// TestServer_StalledClientIsCutOff is the other half of the contract: making
// the deadline about progress must not make it toothless. A client that stops
// reading entirely still has its connection closed, or a stuck peer would pin
// a connection and its goroutine forever.
func TestServer_StalledClientIsCutOff(t *testing.T) {
	const (
		timeout = 300 * time.Millisecond
		size    = 64 << 20
	)

	base := startServer(t, timeout, timeout)

	body := bytes.Repeat([]byte("abcdefgh"), size/8)
	putObject(t, base, "bucket", "big", body)

	addr := base[len("http://"):]

	conn, err := net.Dial("tcp", addr)
	require.NoError(t, err)

	defer func() { _ = conn.Close() }()

	_, err = fmt.Fprintf(conn, "GET /bucket/big HTTP/1.1\r\nHost: %s\r\n\r\n", addr)
	require.NoError(t, err)

	// Read the head, then stop reading entirely for longer than the timeout.
	// The object is far larger than any socket buffer, so the server is still
	// writing when the reads stop and its next write blocks past the deadline.
	head := make([]byte, 4096)
	_, err = conn.Read(head)
	require.NoError(t, err)

	time.Sleep(3 * timeout)

	// Now drain at full speed. What is left is whatever was buffered before
	// the server gave up; the connection must end rather than deliver the
	// whole object.
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(30*time.Second)))

	drained := 0
	buf := make([]byte, 256<<10)

	for {
		n, err := conn.Read(buf)
		drained += n

		if err != nil {
			require.NotErrorIs(t, err, os.ErrDeadlineExceeded,
				"the client's own deadline fired, so the server never closed the connection")
			require.Less(t, drained, size, "server served the whole object to a stalled client")

			return
		}
	}
}
