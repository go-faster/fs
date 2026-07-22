package transport

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/cluster"
)

// Server serves a node's local fragment store to its peers. Wire it onto the
// cluster listener; it is an http.Handler.
type Server struct {
	store  Store
	secret Secret
	mux    *http.ServeMux
	now    func() time.Time
}

// NewServer builds the fragment server for a node-local store.
func NewServer(store Store, secret Secret) *Server {
	s := &Server{
		store:  store,
		secret: secret,
		mux:    http.NewServeMux(),
		now:    time.Now,
	}

	s.mux.HandleFunc("PUT /v1/fragments/{disk}/{name...}", s.put)
	s.mux.HandleFunc("GET /v1/fragments/{disk}/{name...}", s.get)
	s.mux.HandleFunc("HEAD /v1/fragments/{disk}/{name...}", s.stat)
	s.mux.HandleFunc("DELETE /v1/fragments/{disk}/{name...}", s.delete)

	return s
}

// ServeHTTP authenticates the request, then dispatches. The request signature
// is stashed in the header for handlers to bind response signatures to.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqSig, err := s.secret.verifyRequest(r, s.now())
	if err != nil {
		http.Error(w, "cluster auth failed", http.StatusUnauthorized)
		return
	}

	// Re-purpose the header slot: handlers read the VERIFIED signature from
	// here, never the raw client value.
	r.Header.Set(headerAuth, reqSig)

	s.mux.ServeHTTP(w, r)
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
		http.Error(w, "bad fragment path", http.StatusBadRequest)
		return
	}

	wc, err := s.store.Create(r.Context(), disk, name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	hash := sha256.New()

	if _, err := io.Copy(io.MultiWriter(wc, hash), r.Body); err != nil {
		_ = wc.Close()

		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	if err := wc.Close(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
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
		http.Error(w, "bad fragment path", http.StatusBadRequest)
		return
	}

	rc, size, err := s.store.Open(r.Context(), disk, name)
	if err != nil {
		s.storeError(w, err)
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

// stat reports a fragment's size.
func (s *Server) stat(w http.ResponseWriter, r *http.Request) {
	disk, name, ok := target(r)
	if !ok {
		http.Error(w, "bad fragment path", http.StatusBadRequest)
		return
	}

	size, err := s.store.Stat(r.Context(), disk, name)
	if err != nil {
		s.storeError(w, err)
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
		http.Error(w, "bad fragment path", http.StatusBadRequest)
		return
	}

	if err := s.store.Delete(r.Context(), disk, name); err != nil {
		s.storeError(w, err)
		return
	}

	w.Header().Set(headerRespAuth, s.secret.signResponse(r.Header.Get(headerAuth), ""))
	w.WriteHeader(http.StatusNoContent)
}

// storeError maps store errors onto HTTP statuses.
func (s *Server) storeError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNotFound) {
		http.Error(w, "fragment not found", http.StatusNotFound)
		return
	}

	http.Error(w, err.Error(), http.StatusInternalServerError)
}
