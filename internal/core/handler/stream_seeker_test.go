package handler_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/mock"
)

// streamOnly hides every optional interface of the wrapped reader, so a
// GetObjectResponse carrying it is served through the streaming path. It stands
// in for the cluster backend reading a replica off a peer over HTTP, where the
// bytes arrive as a network stream with no way to seek.
type streamOnly struct{ r io.Reader }

func (s streamOnly) Read(p []byte) (int, error) { return s.r.Read(p) } //nolint:wrapcheck // Test shim.
func (s streamOnly) Close() error               { return nil }

// streamedStorage serves body for every key, always through a non-seekable
// reader.
func streamedStorage(body string, modified time.Time) fs.Storage {
	return &mock.StorageMock{
		GetObjectFunc: func(_ context.Context, _, _ string) (*fs.GetObjectResponse, error) {
			return &fs.GetObjectResponse{
				Reader:       streamOnly{r: strings.NewReader(body)},
				Size:         int64(len(body)),
				LastModified: modified,
				ETag:         "d41d8cd98f00b204e9800998ecf8427e",
			}, nil
		},
	}
}

// get issues one request against a handler over the streamed storage.
func get(t *testing.T, svc fs.Storage, method string, headers map[string]string) *http.Response {
	t.Helper()

	req := httptest.NewRequest(method, "/bucket/obj", http.NoBody)
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	newTestHandler(svc).ServeHTTP(rec, req)

	return rec.Result()
}

// TestServeObjectStreamedRange is the regression test for #98: a ranged GET
// answered from a non-seekable reader used to fall through to a path that
// ignored Range entirely and returned 200 with the whole object, so a client
// asking for four bytes silently got the file. Which one happened depended on
// whether the cluster picked a local fragment (an *os.File, seekable) or a
// peer's (an HTTP body, not), making it intermittent in practice.
func TestServeObjectStreamedRange(t *testing.T) {
	const body = "testcontent"

	svc := streamedStorage(body, time.Now().Truncate(time.Second))

	for _, tt := range []struct {
		name        string
		rangeHeader string
		wantStatus  int
		wantBody    string
		wantRange   string
	}{
		{"middle", "bytes=4-7", http.StatusPartialContent, "cont", "bytes 4-7/11"},
		{"leading skipped", "bytes=4-", http.StatusPartialContent, "content", "bytes 4-10/11"},
		{"trailing bytes", "bytes=-7", http.StatusPartialContent, "content", "bytes 4-10/11"},
		{"whole object", "bytes=0-10", http.StatusPartialContent, body, "bytes 0-10/11"},
		{"first byte", "bytes=0-0", http.StatusPartialContent, "t", "bytes 0-0/11"},
		{"no range", "", http.StatusOK, body, ""},
		// Past the end: 416, and the object size in Content-Range.
		{"unsatisfiable", "bytes=100-200", http.StatusRequestedRangeNotSatisfiable, "", "bytes */11"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			headers := map[string]string{}
			if tt.rangeHeader != "" {
				headers["Range"] = tt.rangeHeader
			}

			resp := get(t, svc, http.MethodGet, headers)
			defer func() { _ = resp.Body.Close() }()

			data, err := io.ReadAll(resp.Body)
			require.NoError(t, err)

			require.Equal(t, tt.wantStatus, resp.StatusCode)
			require.Equal(t, tt.wantRange, resp.Header.Get("Content-Range"))

			if tt.wantStatus != http.StatusRequestedRangeNotSatisfiable {
				require.Equal(t, tt.wantBody, string(data))
			}
		})
	}
}

// TestServeObjectStreamedMultiRange pins the deliberate choice for the one
// pattern streamSeeker cannot serve: ServeContent walks the parts of a
// multi-range request in the order the client listed them, which can mean
// seeking backwards. Rather than fail, the Range is dropped and the whole
// object returned — which is what S3 does anyway, supporting only one range per
// request.
func TestServeObjectStreamedMultiRange(t *testing.T) {
	const body = "testcontent"

	svc := streamedStorage(body, time.Now().Truncate(time.Second))

	// Deliberately descending, the case a forward-only reader cannot serve.
	resp := get(t, svc, http.MethodGet, map[string]string{"Range": "bytes=8-10,0-3"})
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, body, string(data))
}

// TestServeObjectStreamedConditional is the regression test for #99: the
// streaming path never evaluated conditional headers, so a GET whose
// precondition should short-circuit returned 200 with the full body instead of
// 304.
func TestServeObjectStreamedConditional(t *testing.T) {
	modified := time.Now().Add(-time.Hour).Truncate(time.Second)
	svc := streamedStorage("testcontent", modified)

	t.Run("if-none-match matching returns 304", func(t *testing.T) {
		resp := get(t, svc, http.MethodGet, map[string]string{
			"If-None-Match": `"d41d8cd98f00b204e9800998ecf8427e"`,
		})
		defer func() { _ = resp.Body.Close() }()

		require.Equal(t, http.StatusNotModified, resp.StatusCode)
	})

	t.Run("if-modified-since after returns 304", func(t *testing.T) {
		resp := get(t, svc, http.MethodGet, map[string]string{
			"If-Modified-Since": modified.Add(time.Minute).UTC().Format(http.TimeFormat),
		})
		defer func() { _ = resp.Body.Close() }()

		require.Equal(t, http.StatusNotModified, resp.StatusCode)
	})

	t.Run("if-none-match not matching returns 200", func(t *testing.T) {
		resp := get(t, svc, http.MethodGet, map[string]string{"If-None-Match": `"other"`})
		defer func() { _ = resp.Body.Close() }()

		data, err := io.ReadAll(resp.Body)
		require.NoError(t, err)

		require.Equal(t, http.StatusOK, resp.StatusCode)
		require.Equal(t, "testcontent", string(data))
	})
}

// TestServeObjectStreamedZeroBytes is the regression test for #100: the
// streaming path only set Content-Length when the size was above zero, so a
// HEAD of an empty object came back without the header at all and boto3 raised
// KeyError: 'ContentLength'. Content-Length: 0 is required — omitting it is not
// the same thing.
func TestServeObjectStreamedZeroBytes(t *testing.T) {
	svc := streamedStorage("", time.Now().Truncate(time.Second))

	for _, method := range []string{http.MethodHead, http.MethodGet} {
		t.Run(method, func(t *testing.T) {
			resp := get(t, svc, method, nil)
			defer func() { _ = resp.Body.Close() }()

			data, err := io.ReadAll(resp.Body)
			require.NoError(t, err)

			require.Equal(t, http.StatusOK, resp.StatusCode)
			require.Equal(t, "0", resp.Header.Get("Content-Length"))
			require.Empty(t, data)
		})
	}
}

// TestServeObjectStreamedHeadDoesNotDrain checks the laziness streamSeeker
// depends on: ServeContent learns the size by seeking to the end, and an eager
// discard would pull the whole object off the network just to answer that. A
// HEAD must not read a single byte.
func TestServeObjectStreamedHeadDoesNotDrain(t *testing.T) {
	var read int

	counting := &mock.StorageMock{
		GetObjectFunc: func(_ context.Context, _, _ string) (*fs.GetObjectResponse, error) {
			return &fs.GetObjectResponse{
				Reader:       streamOnly{r: readCounter{r: strings.NewReader("testcontent"), n: &read}},
				Size:         11,
				LastModified: time.Now().Truncate(time.Second),
			}, nil
		},
	}

	resp := get(t, counting, http.MethodHead, nil)
	defer func() { _ = resp.Body.Close() }()

	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "11", resp.Header.Get("Content-Length"))
	require.Zero(t, read, "HEAD consumed %d bytes of the stream", read)
}

// readCounter tallies bytes actually pulled from the underlying reader.
type readCounter struct {
	r io.Reader
	n *int
}

func (c readCounter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	*c.n += n

	return n, err //nolint:wrapcheck // Test shim.
}
