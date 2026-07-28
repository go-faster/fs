package transport

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-faster/errors"
	"github.com/go-faster/sdk/zctx"
	"go.uber.org/zap"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/reqid"
)

// Server serves a node's local fragment store to its peers, plus — when wired
// with WithStatus — the node's live runtime state. Wire it onto the cluster
// listener; it is an http.Handler.
type Server struct {
	store  Store
	secret Secret
	mux    *http.ServeMux
	now    func() time.Time
	// nodeStatus reports this node's live state; nil serves 501.
	nodeStatus StatusFunc
	// index answers page queries against this node's object index; nil serves
	// 501, which is how a caller learns to read the sidecars instead.
	index IndexFunc
}

// ServerOption configures a Server.
type ServerOption func(*Server)

// WithStatus makes the node serve its live runtime state to peers (GET
// /v1/status), the source the cluster-wide admin view aggregates.
func WithStatus(fn StatusFunc) ServerOption {
	return func(s *Server) { s.nodeStatus = fn }
}

// WithIndex makes the node answer index page queries from peers (GET
// /v1/index), which is what lets a listing cost the page rather than the
// bucket.
func WithIndex(fn IndexFunc) ServerOption {
	return func(s *Server) { s.index = fn }
}

// NewServer builds the fragment server for a node-local store.
func NewServer(store Store, secret Secret, opts ...ServerOption) *Server {
	s := &Server{
		store:  store,
		secret: secret,
		mux:    http.NewServeMux(),
		now:    time.Now,
	}

	for _, opt := range opts {
		opt(s)
	}

	s.mux.HandleFunc("PUT /v1/fragments/{disk}/{name...}", s.put)
	s.mux.HandleFunc("GET /v1/fragments/{disk}/{name...}", s.get)
	s.mux.HandleFunc("HEAD /v1/fragments/{disk}/{name...}", s.stat)
	s.mux.HandleFunc("DELETE /v1/fragments/{disk}/{name...}", s.delete)
	s.mux.HandleFunc("GET /v1/names/{disk}/{prefix...}", s.list)
	s.mux.HandleFunc("GET /v1/status", s.serveStatus)
	s.mux.HandleFunc("GET /v1/index", s.serveIndex)

	return s
}

// ServeHTTP authenticates the request, then dispatches. The request signature
// is stashed in the header for handlers to bind response signatures to.
//
// Authenticated requests carry the sending node and the originating S3 request
// ID onto the context logger, so a failure served here lands in this node's log
// under the same ID the client was handed by the coordinator.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqSig, err := s.secret.verifyRequest(r, s.now())
	if err != nil {
		zctx.From(r.Context()).Warn("Peer request rejected",
			zap.String("peer_node", r.Header.Get(headerNode)),
			zap.String("path", r.URL.Path),
			zap.Error(err),
		)
		http.Error(w, "cluster auth failed", http.StatusUnauthorized)

		return
	}

	// Re-purpose the header slot: handlers read the VERIFIED signature from
	// here, never the raw client value.
	r.Header.Set(headerAuth, reqSig)

	ctx := zctx.With(r.Context(),
		zap.String("peer_node", r.Header.Get(headerNode)),
	)

	// Only tag with an ID when there is a usable one: peer traffic from scrub,
	// repair and rebalance has no client request behind it, and an empty field
	// would read as "correlated to nothing" rather than "not client-originated".
	//
	// An ID that fails validation is dropped rather than logged — logging the
	// value is exactly what an oversized or escape-laden one is for. The length
	// is enough to tell a misbehaving peer from a missing header.
	if id := r.Header.Get(headerRequestID); id != "" {
		if reqid.Valid(id) {
			ctx = reqid.NewContext(ctx, id)
			ctx = zctx.With(ctx, zap.String(reqid.Field, id))
		} else {
			zctx.From(ctx).Warn("Discarding malformed peer request ID",
				zap.Int("length", len(id)),
			)
		}
	}

	s.mux.ServeHTTP(w, r.WithContext(ctx))
}

// fail logs why this node is refusing and answers with err's message.
//
// Every 5xx here is a cause the coordinator cannot see: it receives a bare
// status code, wraps it as "peer returned status 500", and that is all its log
// says. Unless the peer records the underlying error itself, the reason is lost
// on both sides.
func fail(r *http.Request, w http.ResponseWriter, status int, err error) {
	zctx.From(r.Context()).Error("Peer request failed",
		zap.Int("status", status),
		zap.String("method", r.Method),
		zap.String("path", r.URL.Path),
		zap.Error(err),
	)

	http.Error(w, err.Error(), status)
}

// target extracts and validates the (disk, name) pair from the route.
func target(r *http.Request) (cluster.DiskID, string, bool) {
	disk := r.PathValue("disk")
	name := r.PathValue("name")

	if disk == "" || !ValidName(name) {
		return "", "", false
	}

	return cluster.DiskID(disk), name, true
}

// put stores a fragment, responding with the payload digest and a response
// signature over it.
func (s *Server) put(w http.ResponseWriter, r *http.Request) {
	disk, name, ok := target(r)
	if !ok {
		fail(r, w, http.StatusBadRequest, errors.New("bad fragment path"))
		return
	}

	wc, err := s.store.Create(r.Context(), disk, name)
	if err != nil {
		fail(r, w, http.StatusInternalServerError, errors.Wrap(err, "create fragment"))
		return
	}

	hash := sha256.New()

	if _, err := io.Copy(io.MultiWriter(wc, hash), r.Body); err != nil {
		_ = wc.Close()

		fail(r, w, http.StatusInternalServerError, errors.Wrap(err, "stream fragment body"))

		return
	}

	if err := wc.Close(); err != nil {
		fail(r, w, http.StatusInternalServerError, errors.Wrap(err, "commit fragment"))
		return
	}

	digest := hex.EncodeToString(hash.Sum(nil))

	w.Header().Set(headerDigest, digest)
	w.Header().Set(headerRespAuth, s.secret.signResponse(r.Header.Get(headerAuth), digest))
	w.WriteHeader(http.StatusOK)
}

// get streams a fragment, sending its digest and response signature as HTTP
// trailers (hashed while serving — no second read).
func (s *Server) get(w http.ResponseWriter, r *http.Request) {
	disk, name, ok := target(r)
	if !ok {
		fail(r, w, http.StatusBadRequest, errors.New("bad fragment path"))
		return
	}

	rc, size, err := s.store.Open(r.Context(), disk, name)
	if err != nil {
		s.storeError(r, w, err)
		return
	}

	defer func() { _ = rc.Close() }()

	// NB: no Content-Length — trailers require chunked transfer encoding, and
	// an explicit length forces identity encoding, which silently drops them.
	// The size travels in a normal header instead.
	w.Header().Set("Trailer", headerDigest+", "+headerRespAuth)
	w.Header().Set(headerSize, strconv.FormatInt(size, 10))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)

	hash := sha256.New()

	if _, err := io.Copy(io.MultiWriter(w, hash), rc); err != nil {
		// Mid-stream failure: the missing/invalid trailer makes the client
		// reject the payload.
		return
	}

	digest := hex.EncodeToString(hash.Sum(nil))

	w.Header().Set(headerDigest, digest)
	w.Header().Set(headerRespAuth, s.secret.signResponse(r.Header.Get(headerAuth), digest))
}

// list streams the fragment names matching a prefix, newline-separated, with
// the digest and response signature as trailers (same integrity contract as
// get). The prefix travels in the path — not the query — so it is bound into
// the request signature; a tampered prefix cannot silently shrink a listing.
func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	disk := cluster.DiskID(r.PathValue("disk"))
	if disk == "" {
		fail(r, w, http.StatusBadRequest, errors.New("bad list path"))
		return
	}

	names, err := s.store.List(r.Context(), disk, r.PathValue("prefix"))
	if err != nil {
		s.storeError(r, w, err)
		return
	}

	w.Header().Set("Trailer", headerDigest+", "+headerRespAuth)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)

	hash := sha256.New()
	out := io.MultiWriter(w, hash)

	for _, name := range names {
		if _, err := io.WriteString(out, name+"\n"); err != nil {
			// Mid-stream failure: the missing trailer fails the client read.
			return
		}
	}

	digest := hex.EncodeToString(hash.Sum(nil))

	w.Header().Set(headerDigest, digest)
	w.Header().Set(headerRespAuth, s.secret.signResponse(r.Header.Get(headerAuth), digest))
}

// stat reports a fragment's size.
func (s *Server) stat(w http.ResponseWriter, r *http.Request) {
	disk, name, ok := target(r)
	if !ok {
		fail(r, w, http.StatusBadRequest, errors.New("bad fragment path"))
		return
	}

	size, err := s.store.Stat(r.Context(), disk, name)
	if err != nil {
		s.storeError(r, w, err)
		return
	}

	sizeStr := strconv.FormatInt(size, 10)

	w.Header().Set(headerSize, sizeStr)
	w.Header().Set(headerRespAuth, s.secret.signResponse(r.Header.Get(headerAuth), sizeStr))
	w.WriteHeader(http.StatusOK)
}

// delete removes a fragment.
func (s *Server) delete(w http.ResponseWriter, r *http.Request) {
	disk, name, ok := target(r)
	if !ok {
		fail(r, w, http.StatusBadRequest, errors.New("bad fragment path"))
		return
	}

	if err := s.store.Delete(r.Context(), disk, name); err != nil {
		s.storeError(r, w, err)
		return
	}

	w.Header().Set(headerRespAuth, s.secret.signResponse(r.Header.Get(headerAuth), ""))
	w.WriteHeader(http.StatusNoContent)
}

// storeError maps store errors onto HTTP statuses.
func (s *Server) storeError(r *http.Request, w http.ResponseWriter, err error) {
	// A missing fragment is a normal answer (the caller repairs or reads
	// elsewhere), so it stays quiet; anything else is this node failing.
	if errors.Is(err, ErrNotFound) {
		http.Error(w, "fragment not found", http.StatusNotFound)
		return
	}

	fail(r, w, http.StatusInternalServerError, err)
}
