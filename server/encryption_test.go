package server_test

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/sse"
	"github.com/go-faster/fs/server"
	"github.com/go-faster/fs/storagefs"
	"github.com/go-faster/fs/storagemem"
)

const sseHeader = "x-amz-server-side-encryption"

// encryptingServer serves an encrypting storagefs over HTTP.
func encryptingServer(t *testing.T, defaultAlgorithm string) string {
	t.Helper()

	key, err := sse.NewKey()
	require.NoError(t, err)

	mk, err := sse.NewMasterKey(key)
	require.NoError(t, err)

	kr, err := sse.NewKeyring(mk)
	require.NoError(t, err)

	store, err := storagefs.New(t.TempDir(), storagefs.WithEncryption(kr))
	require.NoError(t, err)

	srv, err := server.New(server.Config{
		Storage:           store,
		DefaultEncryption: defaultAlgorithm,
	})
	require.NoError(t, err)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	return ts.URL
}

func put(t *testing.T, url string, body []byte, header map[string]string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(body)) //nolint:noctx // Test client.
	require.NoError(t, err)

	for k, v := range header {
		req.Header.Set(k, v)
	}

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	return resp
}

// TestEncryptionHeaderRoundTrip is the client-visible contract: ask for
// encryption on PUT, and PUT, GET and HEAD all report it.
func TestEncryptionHeaderRoundTrip(t *testing.T) {
	base := encryptingServer(t, "")

	resp := put(t, base+"/test-bucket", nil, nil)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	body := bytes.Repeat([]byte("secret;"), 3000)

	resp = put(t, base+"/test-bucket/k", body, map[string]string{sseHeader: sse.Algorithm})
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, sse.Algorithm, resp.Header.Get(sseHeader), "PUT must echo the algorithm")

	get, err := http.Get(base + "/test-bucket/k") //nolint:noctx // Test client.
	require.NoError(t, err)

	defer func() { _ = get.Body.Close() }()

	require.Equal(t, sse.Algorithm, get.Header.Get(sseHeader), "GET must report the algorithm")

	got, err := io.ReadAll(get.Body)
	require.NoError(t, err)
	require.Equal(t, body, got)
	require.Equal(t, int64(len(body)), get.ContentLength,
		"Content-Length must be the plaintext size, not the stored size")

	head, err := http.Head(base + "/test-bucket/k") //nolint:noctx // Test client.
	require.NoError(t, err)

	_ = head.Body.Close()
	require.Equal(t, sse.Algorithm, head.Header.Get(sseHeader), "HEAD must report the algorithm")
	require.Equal(t, int64(len(body)), head.ContentLength)
}

// TestUnencryptedOmitsHeader: S3 says "not encrypted" by leaving the header
// out, so a client can tell the two apart.
func TestUnencryptedOmitsHeader(t *testing.T) {
	base := encryptingServer(t, "")

	resp := put(t, base+"/test-bucket", nil, nil)
	_ = resp.Body.Close()

	resp = put(t, base+"/test-bucket/plain", []byte("hello"), nil)
	_ = resp.Body.Close()
	require.Empty(t, resp.Header.Get(sseHeader))

	get, err := http.Get(base + "/test-bucket/plain") //nolint:noctx // Test client.
	require.NoError(t, err)

	_ = get.Body.Close()
	require.Empty(t, get.Header.Get(sseHeader))
}

// TestServerDefaultEncrypts covers the deployment-wide switch: an operator
// turns it on and clients that say nothing still get encrypted objects.
func TestServerDefaultEncrypts(t *testing.T) {
	base := encryptingServer(t, sse.Algorithm)

	resp := put(t, base+"/test-bucket", nil, nil)
	_ = resp.Body.Close()

	resp = put(t, base+"/test-bucket/k", []byte("no header sent"), nil)
	_ = resp.Body.Close()
	require.Equal(t, sse.Algorithm, resp.Header.Get(sseHeader))

	get, err := http.Get(base + "/test-bucket/k") //nolint:noctx // Test client.
	require.NoError(t, err)

	defer func() { _ = get.Body.Close() }()

	require.Equal(t, sse.Algorithm, get.Header.Get(sseHeader))

	got, err := io.ReadAll(get.Body)
	require.NoError(t, err)
	require.Equal(t, []byte("no header sent"), got)
}

// TestRangeOverEncrypted is the conformance property a range GET has to keep
// once bodies are ciphertext on disk.
func TestRangeOverEncrypted(t *testing.T) {
	base := encryptingServer(t, sse.Algorithm)

	resp := put(t, base+"/test-bucket", nil, nil)
	_ = resp.Body.Close()

	const size = 3*sse.ChunkSize + 500

	body := make([]byte, size)
	for i := range body {
		body[i] = byte(i)
	}

	resp = put(t, base+"/test-bucket/k", body, nil)
	_ = resp.Body.Close()

	for _, w := range []struct{ first, last int }{
		{0, 9},
		{sse.ChunkSize - 5, sse.ChunkSize + 5},
		{2 * sse.ChunkSize, 2*sse.ChunkSize + 100},
		{size - 10, size - 1},
	} {
		req, err := http.NewRequest(http.MethodGet, base+"/test-bucket/k", http.NoBody) //nolint:noctx // Test client.
		require.NoError(t, err)
		req.Header.Set("Range", byteRange(w.first, w.last))

		got, err := http.DefaultClient.Do(req)
		require.NoError(t, err)

		require.Equal(t, http.StatusPartialContent, got.StatusCode, "range %d-%d", w.first, w.last)

		data, err := io.ReadAll(got.Body)
		_ = got.Body.Close()

		require.NoError(t, err)
		require.Equal(t, body[w.first:w.last+1], data, "range %d-%d", w.first, w.last)
	}
}

func byteRange(first, last int) string {
	return "bytes=" + strconv.Itoa(first) + "-" + strconv.Itoa(last)
}

func doReq(t *testing.T, method, url, body string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(method, url, strings.NewReader(body)) //nolint:noctx // Test client.
	require.NoError(t, err)

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	return resp
}

const encryptionDoc = `<ServerSideEncryptionConfiguration>` +
	`<Rule><ApplyServerSideEncryptionByDefault><SSEAlgorithm>AES256</SSEAlgorithm>` +
	`</ApplyServerSideEncryptionByDefault></Rule></ServerSideEncryptionConfiguration>`

// TestBucketEncryptionSubresource covers the ?encryption round trip and the
// state S3 reports before anything is configured.
func TestBucketEncryptionSubresource(t *testing.T) {
	base := encryptingServer(t, "")

	resp := put(t, base+"/test-bucket", nil, nil)
	_ = resp.Body.Close()

	// Unset is a distinct answer, not an empty configuration.
	get := doReq(t, http.MethodGet, base+"/test-bucket?encryption", "")
	body, err := io.ReadAll(get.Body)
	_ = get.Body.Close()

	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, get.StatusCode)
	require.Contains(t, string(body), "ServerSideEncryptionConfigurationNotFoundError")

	set := doReq(t, http.MethodPut, base+"/test-bucket?encryption", encryptionDoc)
	_ = set.Body.Close()
	require.Equal(t, http.StatusOK, set.StatusCode)

	get = doReq(t, http.MethodGet, base+"/test-bucket?encryption", "")
	body, err = io.ReadAll(get.Body)
	_ = get.Body.Close()

	require.NoError(t, err)
	require.Equal(t, http.StatusOK, get.StatusCode)
	require.Contains(t, string(body), "<SSEAlgorithm>AES256</SSEAlgorithm>")

	del := doReq(t, http.MethodDelete, base+"/test-bucket?encryption", "")
	_ = del.Body.Close()
	require.Equal(t, http.StatusNoContent, del.StatusCode)

	get = doReq(t, http.MethodGet, base+"/test-bucket?encryption", "")
	_ = get.Body.Close()
	require.Equal(t, http.StatusNotFound, get.StatusCode, "delete must leave the bucket unconfigured")
}

// TestBucketDefaultEncrypts is what the subresource is for: objects written
// with no header are encrypted because the bucket says so.
func TestBucketDefaultEncrypts(t *testing.T) {
	base := encryptingServer(t, "")

	resp := put(t, base+"/test-bucket", nil, nil)
	_ = resp.Body.Close()

	// Before the default, a plain PUT is unencrypted.
	resp = put(t, base+"/test-bucket/before", []byte("plain"), nil)
	_ = resp.Body.Close()
	require.Empty(t, resp.Header.Get(sseHeader))

	set := doReq(t, http.MethodPut, base+"/test-bucket?encryption", encryptionDoc)
	_ = set.Body.Close()
	require.Equal(t, http.StatusOK, set.StatusCode)

	resp = put(t, base+"/test-bucket/after", []byte("secret"), nil)
	_ = resp.Body.Close()
	require.Equal(t, sse.Algorithm, resp.Header.Get(sseHeader),
		"the bucket default must apply to a request that names nothing")

	get, err := http.Get(base + "/test-bucket/after") //nolint:noctx // Test client.
	require.NoError(t, err)

	defer func() { _ = get.Body.Close() }()

	require.Equal(t, sse.Algorithm, get.Header.Get(sseHeader))

	got, err := io.ReadAll(get.Body)
	require.NoError(t, err)
	require.Equal(t, []byte("secret"), got)

	// The object written before the default stays unencrypted and readable:
	// setting a default is not a retroactive rewrite.
	old, err := http.Get(base + "/test-bucket/before") //nolint:noctx // Test client.
	require.NoError(t, err)

	defer func() { _ = old.Body.Close() }()

	require.Empty(t, old.Header.Get(sseHeader))
}

// TestBucketEncryptionRejectsUnsupported: a bucket must not be left configured
// for an encryption this server will never perform.
func TestBucketEncryptionRejectsUnsupported(t *testing.T) {
	base := encryptingServer(t, "")

	resp := put(t, base+"/test-bucket", nil, nil)
	_ = resp.Body.Close()

	kms := `<ServerSideEncryptionConfiguration><Rule>` +
		`<ApplyServerSideEncryptionByDefault><SSEAlgorithm>aws:kms</SSEAlgorithm>` +
		`</ApplyServerSideEncryptionByDefault></Rule></ServerSideEncryptionConfiguration>`

	set := doReq(t, http.MethodPut, base+"/test-bucket?encryption", kms)
	_ = set.Body.Close()
	require.Equal(t, http.StatusBadRequest, set.StatusCode)

	get := doReq(t, http.MethodGet, base+"/test-bucket?encryption", "")
	_ = get.Body.Close()
	require.Equal(t, http.StatusNotFound, get.StatusCode,
		"a refused configuration must not have been stored")
}

// TestHeaderBeatsBucketDefault pins the precedence: most specific wins.
func TestHeaderBeatsBucketDefault(t *testing.T) {
	base := encryptingServer(t, "")

	resp := put(t, base+"/test-bucket", nil, nil)
	_ = resp.Body.Close()

	set := doReq(t, http.MethodPut, base+"/test-bucket?encryption", encryptionDoc)
	_ = set.Body.Close()

	// An explicit algorithm this server cannot perform must be refused even
	// though the bucket default would have succeeded.
	resp = put(t, base+"/test-bucket/k", []byte("x"), map[string]string{sseHeader: "aws:kms"})
	_ = resp.Body.Close()
	require.NotEqual(t, http.StatusOK, resp.StatusCode,
		"an explicit unsupported algorithm must not fall back to the bucket default")
}

// TestBucketEncryptionUnsupportedBackend: a backend that cannot encrypt must
// report ?encryption as NotImplemented, not as a server error and not as an
// empty configuration a client would read as "off".
func TestBucketEncryptionUnsupportedBackend(t *testing.T) {
	srv, err := server.New(server.Config{Storage: storagemem.New()})
	require.NoError(t, err)

	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)

	resp := put(t, ts.URL+"/test-bucket", nil, nil)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	for _, tc := range []struct {
		method string
		body   string
	}{
		{http.MethodGet, ""},
		{http.MethodPut, encryptionDoc},
		{http.MethodDelete, ""},
	} {
		got := doReq(t, tc.method, ts.URL+"/test-bucket?encryption", tc.body)
		body, err := io.ReadAll(got.Body)
		_ = got.Body.Close()

		require.NoError(t, err)
		require.Equal(t, http.StatusNotImplemented, got.StatusCode, "%s ?encryption", tc.method)
		require.Contains(t, string(body), "NotImplemented")
	}

	// And a plain write still works: the bucket has no default to consult.
	obj := put(t, ts.URL+"/test-bucket/k", []byte("plain"), nil)
	_ = obj.Body.Close()
	require.Equal(t, http.StatusOK, obj.StatusCode)
	require.Empty(t, obj.Header.Get(sseHeader))
}
