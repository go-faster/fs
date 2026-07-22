package transport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"hash"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster"
)

// Client talks to one peer's fragment server. It is safe for concurrent use.
type Client struct {
	base   string // peer base URL, e.g. http://10.0.0.2:7000
	secret Secret
	node   cluster.NodeID // sender identity
	http   *http.Client
	now    func() time.Time
}

// NewClient builds a client for the peer at baseURL, authenticating as node.
// httpClient may be nil for http.DefaultClient.
func NewClient(baseURL string, secret Secret, node cluster.NodeID, httpClient *http.Client) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	return &Client{base: baseURL, secret: secret, node: node, http: httpClient, now: time.Now}
}

// fragmentURL builds the peer URL for a fragment, escaping each name segment.
func (c *Client) fragmentURL(disk cluster.DiskID, name string) string {
	return c.base + "/v1/fragments/" + url.PathEscape(string(disk)) + "/" + escapeName(name)
}

// escapeName escapes a slash-separated fragment name segment by segment,
// keeping the slashes routable.
func escapeName(name string) string {
	segs := strings.Split(name, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}

	return strings.Join(segs, "/")
}

// Put streams size bytes from body to the peer as the named fragment. The
// payload is hashed while streaming; the peer's acknowledged digest and
// response signature are verified, so a successful return means the peer
// durably holds exactly these bytes.
func (c *Client) Put(ctx context.Context, disk cluster.DiskID, name string, size int64, body io.Reader) error {
	if !ValidName(name) {
		return errors.Errorf("invalid fragment name %q", name)
	}

	digester := sha256.New()

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, c.fragmentURL(disk, name), io.TeeReader(body, digester))
	if err != nil {
		return errors.Wrap(err, "build request")
	}

	req.ContentLength = size

	reqSig := c.secret.authenticate(req, c.node, c.now())

	resp, err := c.http.Do(req)
	if err != nil {
		return errors.Wrap(err, "put fragment")
	}

	defer drainClose(resp)

	if err := statusError(resp); err != nil {
		return err
	}

	digest := resp.Header.Get(headerDigest)
	if err := c.secret.verifyResponse(reqSig, digest, resp.Header.Get(headerRespAuth)); err != nil {
		return err
	}

	if digest != hex.EncodeToString(digester.Sum(nil)) {
		return errors.Wrap(ErrChecksumMismatch, "peer stored different bytes")
	}

	return nil
}

// Get opens the named fragment on the peer, returning its payload and size.
// The payload digest arrives as an HTTP trailer and is verified at EOF: the
// reader returns ErrChecksumMismatch (instead of io.EOF) if the bytes read do
// not match what the peer signed.
func (c *Client) Get(ctx context.Context, disk cluster.DiskID, name string) (io.ReadCloser, int64, error) {
	if !ValidName(name) {
		return nil, 0, errors.Errorf("invalid fragment name %q", name)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.fragmentURL(disk, name), http.NoBody)
	if err != nil {
		return nil, 0, errors.Wrap(err, "build request")
	}

	reqSig := c.secret.authenticate(req, c.node, c.now())

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, 0, errors.Wrap(err, "get fragment")
	}

	if err := statusError(resp); err != nil {
		drainClose(resp)
		return nil, 0, err
	}

	// Size travels in a header: the response is chunked (trailers require it),
	// so ContentLength is unset.
	size, err := strconv.ParseInt(resp.Header.Get(headerSize), 10, 64)
	if err != nil {
		drainClose(resp)
		return nil, 0, errors.Wrap(err, "parse fragment size")
	}

	return &verifyingReader{
		resp:   resp,
		secret: c.secret,
		reqSig: reqSig,
		hash:   sha256.New(),
	}, size, nil
}

// Stat reports the size of the named fragment on the peer.
func (c *Client) Stat(ctx context.Context, disk cluster.DiskID, name string) (int64, error) {
	if !ValidName(name) {
		return 0, errors.Errorf("invalid fragment name %q", name)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, c.fragmentURL(disk, name), http.NoBody)
	if err != nil {
		return 0, errors.Wrap(err, "build request")
	}

	reqSig := c.secret.authenticate(req, c.node, c.now())

	resp, err := c.http.Do(req)
	if err != nil {
		return 0, errors.Wrap(err, "stat fragment")
	}

	defer drainClose(resp)

	if err := statusError(resp); err != nil {
		return 0, err
	}

	sizeStr := resp.Header.Get(headerSize)
	if err := c.secret.verifyResponse(reqSig, sizeStr, resp.Header.Get(headerRespAuth)); err != nil {
		return 0, err
	}

	size, err := strconv.ParseInt(sizeStr, 10, 64)
	if err != nil {
		return 0, errors.Wrap(err, "parse fragment size")
	}

	return size, nil
}

// Delete removes the named fragment on the peer.
func (c *Client) Delete(ctx context.Context, disk cluster.DiskID, name string) error {
	if !ValidName(name) {
		return errors.Errorf("invalid fragment name %q", name)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.fragmentURL(disk, name), http.NoBody)
	if err != nil {
		return errors.Wrap(err, "build request")
	}

	reqSig := c.secret.authenticate(req, c.node, c.now())

	resp, err := c.http.Do(req)
	if err != nil {
		return errors.Wrap(err, "delete fragment")
	}

	defer drainClose(resp)

	if err := statusError(resp); err != nil {
		return err
	}

	return c.secret.verifyResponse(reqSig, "", resp.Header.Get(headerRespAuth))
}

// verifyingReader streams a GET body while hashing, and at EOF verifies the
// peer's trailer digest and response signature before reporting a clean end.
type verifyingReader struct {
	resp   *http.Response
	secret Secret
	reqSig string
	hash   hash.Hash
	failed error
}

func (v *verifyingReader) Read(p []byte) (int, error) {
	if v.failed != nil {
		return 0, v.failed
	}

	n, err := v.resp.Body.Read(p)
	if n > 0 {
		_, _ = v.hash.Write(p[:n])
	}

	if errors.Is(err, io.EOF) {
		if verr := v.verify(); verr != nil {
			v.failed = verr
			return n, verr
		}
	}

	return n, err
}

// verify checks the trailer digest and signature after the body is consumed.
func (v *verifyingReader) verify() error {
	digest := v.resp.Trailer.Get(headerDigest)
	if digest == "" {
		return errors.Wrap(ErrChecksumMismatch, "peer sent no digest trailer (truncated stream?)")
	}

	if err := v.secret.verifyResponse(v.reqSig, digest, v.resp.Trailer.Get(headerRespAuth)); err != nil {
		return err
	}

	if digest != hex.EncodeToString(v.hash.Sum(nil)) {
		return errors.Wrap(ErrChecksumMismatch, "payload digest mismatch")
	}

	return nil
}

func (v *verifyingReader) Close() error {
	return v.resp.Body.Close()
}

// statusError maps response statuses onto sentinel errors.
func statusError(resp *http.Response) error {
	switch resp.StatusCode {
	case http.StatusOK, http.StatusNoContent:
		return nil
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusUnauthorized:
		return ErrUnauthorized
	default:
		return errors.Errorf("peer returned status %d", resp.StatusCode)
	}
}

// drainClose discards any remaining body and closes it, keeping the
// connection reusable.
func drainClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
}
