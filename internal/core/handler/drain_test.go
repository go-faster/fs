package handler_test

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/core/handler"
)

// TestRefusedBodyDrained checks that a write refused before its body is read
// still lets the client send the body and read the refusal, as aws-sdk-go-v2
// does: it announces Expect: 100-continue and sends the body without waiting.
// The client here reads the response before sending the body and sends it
// after the server would have closed the connection, so a server that does
// not drain fails it every time rather than by a race.
func TestRefusedBodyDrained(t *testing.T) {
	svc := baseMock()
	svc.PutObjectFunc = func(context.Context, *fs.PutObjectRequest) (*fs.PutObjectResponse, error) {
		return nil, fs.ErrQuotaExceeded
	}

	srv := httptest.NewServer(handler.New(svc))
	t.Cleanup(srv.Close)

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	const size = 8 << 20

	_, err = fmt.Fprintf(conn, "PUT /b/k HTTP/1.1\r\nHost: s3\r\nContent-Length: %d\r\nExpect: 100-continue\r\n\r\n", size)
	require.NoError(t, err)

	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	require.NoError(t, err)
	assert.Equal(t, http.StatusForbidden, resp.StatusCode, "the refusal, with no 100 Continue before it")
	require.NoError(t, resp.Body.Close())

	// Past the server's half-second wait before it closes a connection.
	time.Sleep(time.Second)

	_, err = io.Copy(conn, io.LimitReader(zeros{}, size))
	require.NoError(t, err, "the body is read, not reset")

	_, err = conn.Read(make([]byte, 1))
	require.ErrorIs(t, err, io.EOF, "and the connection closed cleanly")
}

func TestRefusedBodyTooLarge(t *testing.T) {
	svc := baseMock()
	svc.PutObjectFunc = func(context.Context, *fs.PutObjectRequest) (*fs.PutObjectResponse, error) {
		return nil, fs.ErrQuotaExceeded
	}

	srv := httptest.NewServer(handler.New(svc))
	t.Cleanup(srv.Close)

	req, err := http.NewRequest(http.MethodPut, srv.URL+"/b/k", strings.NewReader(""))
	require.NoError(t, err)

	// Declared past the limit and never sent: the server answers without
	// waiting for it.
	req.ContentLength = 1 << 40
	req.Body = io.NopCloser(blocking{})

	done := make(chan *http.Response, 1)

	go func() {
		resp, err := srv.Client().Transport.(*http.Transport).RoundTrip(req)
		if err == nil {
			_ = resp.Body.Close()
		}

		done <- resp
	}()

	select {
	case resp := <-done:
		if resp != nil {
			assert.Equal(t, http.StatusForbidden, resp.StatusCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the server waited for a body too large to drain")
	}
}

type zeros struct{}

func (zeros) Read(p []byte) (int, error) {
	clear(p)

	return len(p), nil
}

// blocking is a body that never yields a byte.
type blocking struct{}

func (blocking) Read([]byte) (int, error) { select {} }
