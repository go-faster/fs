package peer

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var testSecret = Secret("cluster-secret-0123456789")

// echo answers with the request body, prefixed.
var echo = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(r.Body)
	_, _ = w.Write(append([]byte("got "), b...))
})

// tamper is a RoundTripper that rewrites requests or responses in flight.
type tamper struct {
	req  func(*http.Request)
	resp func(*http.Response)
}

func (t tamper) RoundTrip(r *http.Request) (*http.Response, error) {
	if t.req != nil {
		t.req(r)
	}

	resp, err := http.DefaultTransport.RoundTrip(r)
	if err == nil && t.resp != nil {
		t.resp(resp)
	}

	return resp, err
}

func client(secret Secret, base http.RoundTripper, now func() time.Time) *http.Client {
	return &http.Client{Transport: &transport{secret: secret, node: "n1", base: base, now: now}}
}

func post(t *testing.T, c *http.Client, url, body string) (string, error) {
	t.Helper()

	resp, err := c.Post(url+"/x?a=1", "text/plain", strings.NewReader(body))
	if err != nil {
		return "", err
	}

	defer func() { _ = resp.Body.Close() }()

	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)

	return string(b), nil
}

func TestAuthRoundTrip(t *testing.T) {
	srv := httptest.NewServer(testSecret.Handler(echo, time.Now))
	defer srv.Close()

	got, err := post(t, client(testSecret, http.DefaultTransport, time.Now), srv.URL, "hello")
	require.NoError(t, err)
	assert.Equal(t, "got hello", got)
}

func TestAuthRejects(t *testing.T) {
	srv := httptest.NewServer(testSecret.Handler(echo, time.Now))
	defer srv.Close()

	for name, c := range map[string]*http.Client{
		"wrong secret": client(Secret("another-secret-0123456789"), http.DefaultTransport, time.Now),
		"clock behind": client(testSecret, http.DefaultTransport, func() time.Time { return time.Now().Add(-2 * maxSkew) }),
		"body changed": client(testSecret, tamper{req: func(r *http.Request) {
			r.Body = io.NopCloser(strings.NewReader("evil!"))
		}}, time.Now),
		"query changed": client(testSecret, tamper{req: func(r *http.Request) {
			r.URL.RawQuery = "a=2"
		}}, time.Now),
		"node changed": client(testSecret, tamper{req: func(r *http.Request) {
			r.Header.Set(headerNode, "n2")
		}}, time.Now),
		"response changed": client(testSecret, tamper{resp: func(r *http.Response) {
			r.Body = io.NopCloser(bytes.NewReader([]byte("got evil!")))
		}}, time.Now),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := post(t, c, srv.URL, "hello")
			require.ErrorIs(t, err, ErrUnauthorized)
		})
	}
}

func TestAuthUnsignedRequest(t *testing.T) {
	srv := httptest.NewServer(testSecret.Handler(echo, time.Now))
	defer srv.Close()

	resp, err := http.Post(srv.URL, "text/plain", strings.NewReader("hello"))
	require.NoError(t, err)

	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
}
