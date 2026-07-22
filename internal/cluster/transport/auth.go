package transport

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster"
)

// Secret is the shared cluster secret both sides of the transport hold. It
// authenticates requests and responses (mutual auth); rotating it is a rolling
// restart concern for now.
type Secret []byte

// signRequest computes the request HMAC binding method, path, timestamp and the
// sending node.
func (s Secret) signRequest(method, path, timestamp string, node cluster.NodeID) string {
	mac := hmac.New(sha256.New, s)
	_, _ = mac.Write([]byte(method))
	_, _ = mac.Write([]byte{'\n'})
	_, _ = mac.Write([]byte(path))
	_, _ = mac.Write([]byte{'\n'})
	_, _ = mac.Write([]byte(timestamp))
	_, _ = mac.Write([]byte{'\n'})
	_, _ = mac.Write([]byte(node))

	return hex.EncodeToString(mac.Sum(nil))
}

// signResponse computes the response HMAC binding the request signature and the
// payload digest, proving the responder holds the secret AND handled these
// exact bytes for this exact request.
func (s Secret) signResponse(requestSig, digest string) string {
	mac := hmac.New(sha256.New, s)
	_, _ = mac.Write([]byte(requestSig))
	_, _ = mac.Write([]byte{'\n'})
	_, _ = mac.Write([]byte(digest))

	return hex.EncodeToString(mac.Sum(nil))
}

// authenticate stamps an outgoing request with timestamp, node and signature,
// returning the request signature for response verification.
func (s Secret) authenticate(r *http.Request, node cluster.NodeID, now time.Time) string {
	ts := strconv.FormatInt(now.Unix(), 10)
	sig := s.signRequest(r.Method, r.URL.Path, ts, node)

	r.Header.Set(headerTimestamp, ts)
	r.Header.Set(headerNode, string(node))
	r.Header.Set(headerAuth, sig)

	return sig
}

// verifyRequest checks an incoming request's signature and timestamp skew,
// returning the request signature (for response signing) on success.
func (s Secret) verifyRequest(r *http.Request, now time.Time) (string, error) {
	ts := r.Header.Get(headerTimestamp)
	node := r.Header.Get(headerNode)
	got := r.Header.Get(headerAuth)

	if ts == "" || node == "" || got == "" {
		return "", errors.Wrap(ErrUnauthorized, "missing auth headers")
	}

	unix, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return "", errors.Wrap(ErrUnauthorized, "bad timestamp")
	}

	if skew := now.Unix() - unix; skew > maxClockSkew || skew < -maxClockSkew {
		return "", errors.Wrap(ErrUnauthorized, "timestamp outside allowed skew")
	}

	want := s.signRequest(r.Method, r.URL.Path, ts, cluster.NodeID(node))
	if !hmac.Equal([]byte(want), []byte(got)) {
		return "", errors.Wrap(ErrUnauthorized, "bad request signature")
	}

	return want, nil
}

// verifyResponse checks a response HMAC against the request signature and
// payload digest.
func (s Secret) verifyResponse(requestSig, digest, got string) error {
	want := s.signResponse(requestSig, digest)
	if !hmac.Equal([]byte(want), []byte(got)) {
		return errors.Wrap(ErrUnauthorized, "bad response signature")
	}

	return nil
}
