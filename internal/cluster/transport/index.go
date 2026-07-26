package transport

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-faster/errors"
)

// maxIndexBody bounds the index page a peer may return. A page is bounded by
// the caller's limit, but the limit is the caller's claim: this is the reader's
// own bound, applied before a byte of an unverified response is trusted.
const maxIndexBody = 8 << 20

// maxIndexLimit caps how many entries one request may ask for, so a peer cannot
// be made to assemble an unbounded page.
const maxIndexLimit = 10_000

// IndexQuery asks a peer for one page of the objects it holds.
type IndexQuery struct {
	Bucket string
	// Prefix restricts the page; empty asks for the bucket.
	Prefix string
	// After is an exclusive lower bound in key order.
	After string
	// Limit bounds the page. The peer may return fewer.
	Limit int
}

// IndexEntry is one object as the answering node holds it. It carries the
// ordering stamps because a merge across nodes has to decide which copy of a
// key is current, by the same rule the sidecars themselves use.
type IndexEntry struct {
	Key        string    `json:"key"`
	Size       int64     `json:"size"`
	ETag       string    `json:"etag,omitempty"`
	Modified   time.Time `json:"modified"`
	Seq        int64     `json:"seq,omitempty"`
	Generation string    `json:"generation,omitempty"`
	OwnerID    string    `json:"owner_id,omitempty"`
	OwnerName  string    `json:"owner_name,omitempty"`
}

// IndexPage is a peer's answer.
type IndexPage struct {
	// Ready reports whether the node's index is usable. A node still building
	// one answers false and no entries: its objects would simply be missing
	// from a merge, and a listing short of keys is worse than a slow one, so
	// the caller falls back to reading the sidecars instead.
	Ready bool `json:"ready"`
	// Entries are sorted by key, ascending.
	Entries []IndexEntry `json:"entries,omitempty"`
}

// IndexFunc answers an index query from the node's own index.
type IndexFunc func(ctx context.Context, q IndexQuery) (IndexPage, error)

// serveIndex returns a page of the node's index as a signed JSON document,
// under the same contract as every other peer response: the signature covers
// the body digest, so a tampered page cannot be passed off as this node's.
func (s *Server) serveIndex(w http.ResponseWriter, r *http.Request) {
	if s.index == nil {
		http.Error(w, "object index not served here", http.StatusNotImplemented)
		return
	}

	q := r.URL.Query()

	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil || limit <= 0 {
		http.Error(w, "invalid limit", http.StatusBadRequest)
		return
	}

	page, err := s.index(r.Context(), IndexQuery{
		Bucket: q.Get("bucket"),
		Prefix: q.Get("prefix"),
		After:  q.Get("after"),
		Limit:  min(limit, maxIndexLimit),
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	body, err := json.Marshal(page)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	sum := sha256.Sum256(body)
	digest := hex.EncodeToString(sum[:])

	w.Header().Set(headerDigest, digest)
	w.Header().Set(headerRespAuth, s.secret.signResponse(r.Header.Get(headerAuth), digest))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)

	_, _ = w.Write(body)
}

// Index fetches one page of a peer's object index.
//
// A peer that does not serve it — an older binary (no route) or one wired
// without an index — reports ErrUnsupported, so a rolling upgrade degrades to
// listing from the sidecars rather than to an error.
func (c *Client) IndexPage(ctx context.Context, q IndexQuery) (IndexPage, error) {
	u := *c.base
	u.Path = strings.TrimSuffix(c.base.Path, "/") + "/v1/index"
	u.RawPath = ""

	values := url.Values{}
	values.Set("bucket", q.Bucket)
	values.Set("limit", strconv.Itoa(q.Limit))

	if q.Prefix != "" {
		values.Set("prefix", q.Prefix)
	}

	if q.After != "" {
		values.Set("after", q.After)
	}

	u.RawQuery = values.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), http.NoBody)
	if err != nil {
		return IndexPage{}, errors.Wrap(err, "build request")
	}

	reqSig := c.secret.authenticate(req, c.node, c.now())

	resp, err := c.http.Do(req)
	if err != nil {
		return IndexPage{}, errors.Wrap(err, "get index page")
	}

	defer drainClose(resp)

	if err := statusError(resp); err != nil {
		if errors.Is(err, ErrNotFound) {
			return IndexPage{}, ErrUnsupported
		}

		return IndexPage{}, err
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxIndexBody))
	if err != nil {
		return IndexPage{}, errors.Wrap(err, "read index page")
	}

	digest := resp.Header.Get(headerDigest)
	if err := c.secret.verifyResponse(reqSig, digest, resp.Header.Get(headerRespAuth)); err != nil {
		return IndexPage{}, err
	}

	sum := sha256.Sum256(body)
	if digest != hex.EncodeToString(sum[:]) {
		return IndexPage{}, errors.Wrap(ErrChecksumMismatch, "index page digest mismatch")
	}

	var page IndexPage
	if err := json.Unmarshal(body, &page); err != nil {
		return IndexPage{}, errors.Wrap(err, "decode index page")
	}

	return page, nil
}
