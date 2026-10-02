package peer

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"strconv"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster/layout"
)

// Header names of the peer authentication protocol.
const (
	headerNode      = "X-Fs-Node"
	headerTimestamp = "X-Fs-Timestamp"
	headerAuth      = "X-Fs-Auth"
	headerRespAuth  = "X-Fs-Response-Auth"
)

// maxSkew bounds how far a request's timestamp may be from the receiver's
// clock. It is also the window in which a captured request can be replayed.
//
// ponytail: replay within the window is accepted; every operation today is
// idempotent (a status read, a layout the receiver adopts only if newer). Add a
// nonce cache before a non-idempotent operation is exposed.
const maxSkew = 60 * time.Second

// maxBody bounds a request or response body. Both are buffered to be signed.
const maxBody = 64 << 20

// ErrUnauthorized is returned when a request or response fails authentication.
var ErrUnauthorized = errors.New("peer authentication failed")

// Secret is the shared cluster secret. It authenticates requests and
// responses in both directions: each side proves it holds the secret, and that
// the bytes it signed are the bytes that arrived. It does not encrypt — run
// peer traffic on a private network.
type Secret []byte

// sign is the HMAC over the request's identity and body. uri is the path and
// query: everything the receiver acts on is signed.
func (s Secret) sign(method, uri, timestamp string, node layout.NodeID, body []byte) string {
	sum := sha256.Sum256(body)

	mac := hmac.New(sha256.New, s)
	for _, part := range []string{method, uri, timestamp, string(node), hex.EncodeToString(sum[:])} {
		_, _ = mac.Write([]byte(part))
		_, _ = mac.Write([]byte{'\n'})
	}

	return hex.EncodeToString(mac.Sum(nil))
}

// signResponse binds a response body to the request it answers.
func (s Secret) signResponse(requestSig string, status int, body []byte) string {
	return s.sign(strconv.Itoa(status), requestSig, "", "", body)
}

// Handler authenticates every request to h and signs every response, so a
// peer can trust the answer as much as the server trusts the question. The
// verified sending node is in the X-Fs-Node header.
func (s Secret) Handler(h http.Handler, now func() time.Time) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)

			return
		}

		sig, err := s.verify(r, body, now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnauthorized)

			return
		}

		r.Body = io.NopCloser(bytes.NewReader(body))

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)

		maps.Copy(w.Header(), rec.Header())

		w.Header().Set(headerRespAuth, s.signResponse(sig, rec.Code, rec.Body.Bytes()))
		w.WriteHeader(rec.Code)
		_, _ = w.Write(rec.Body.Bytes())
	})
}

func (s Secret) verify(r *http.Request, body []byte, now time.Time) (string, error) {
	ts, node, got := r.Header.Get(headerTimestamp), r.Header.Get(headerNode), r.Header.Get(headerAuth)
	if ts == "" || node == "" || got == "" {
		return "", errors.Wrap(ErrUnauthorized, "missing headers")
	}

	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return "", errors.Wrap(ErrUnauthorized, "bad timestamp")
	}

	if skew := now.Sub(time.Unix(unix, 0)); skew > maxSkew || skew < -maxSkew {
		return "", errors.Wrap(ErrUnauthorized, "timestamp outside allowed skew")
	}

	want := s.sign(r.Method, r.URL.RequestURI(), ts, layout.NodeID(node), body)
	if !hmac.Equal([]byte(want), []byte(got)) {
		return "", errors.Wrap(ErrUnauthorized, "bad signature")
	}

	return want, nil
}

// transport signs requests as node and verifies the responses.
type transport struct {
	secret Secret
	node   layout.NodeID
	base   http.RoundTripper
	now    func() time.Time
}

func (t *transport) RoundTrip(r *http.Request) (*http.Response, error) {
	var body []byte

	if r.Body != nil {
		var err error

		body, err = io.ReadAll(io.LimitReader(r.Body, maxBody))
		_ = r.Body.Close()

		if err != nil {
			return nil, errors.Wrap(err, "read request body")
		}
	}

	r = r.Clone(r.Context())
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))

	ts := strconv.FormatInt(t.now().Unix(), 10)
	sig := t.secret.sign(r.Method, r.URL.RequestURI(), ts, t.node, body)

	r.Header.Set(headerTimestamp, ts)
	r.Header.Set(headerNode, string(t.node))
	r.Header.Set(headerAuth, sig)

	resp, err := t.base.RoundTrip(r)
	if err != nil {
		return nil, err
	}

	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, errors.Wrap(err, "read response body")
	}

	want := t.secret.signResponse(sig, resp.StatusCode, respBody)
	if !hmac.Equal([]byte(want), []byte(resp.Header.Get(headerRespAuth))) {
		return nil, errors.Wrapf(ErrUnauthorized, "response from %s (status %d)", r.URL.Host, resp.StatusCode)
	}

	resp.Body = io.NopCloser(bytes.NewReader(respBody))

	return resp, nil
}
