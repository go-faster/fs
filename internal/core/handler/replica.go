package handler

import (
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/s3err"
)

// The ?x-fs-replica bucket subresource: the target side of geo-replication
// (docs/design/geo-replication.md), spoken between fs deployments only.
// Requests need the admin grant on the bucket (requestScope); bodies and
// answers are JSON of the fs.Replica* types.
//
//	GET  ?x-fs-replica                    fs.ReplicaInfo
//	POST ?x-fs-replica=blocks             claimRequest → []fs.ReplicaBlockClaim
//	PUT  ?x-fs-replica=block&version=V&hash=H&scheme=S   the block's bytes
//	POST ?x-fs-replica=versions           []fs.ReplicaVersion → []fs.ReplicaResult
const replicaParam = "x-fs-replica"

// maxReplicaBlock bounds a block's body: the block layer's limit.
const maxReplicaBlock = 32 << 20

// maxReplicaJSON bounds a claim or a batch of versions. Versions carry
// inline content of a few KiB each at most, so a batch of a thousand fits
// with room to spare.
const maxReplicaJSON = 64 << 20

// claimRequest is the body of ?x-fs-replica=blocks: the blocks of one
// version.
type claimRequest struct {
	VersionID string            `json:"version_id"`
	Blocks    []fs.ReplicaBlock `json:"blocks"`
}

// Replica serves the ?x-fs-replica subresource.
func (h *handler) Replica(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	bucket, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	q := r.URL.Query()

	rr, ok := h.service.(fs.ReplicaReceiver)
	if !ok {
		s3err.WriteAPI(w, r, s3err.NotImplemented)

		return
	}

	switch op := q.Get(replicaParam); {
	case r.Method == http.MethodGet && op == "":
		info, err := rr.ReplicaInfo(ctx, bucket)
		if err != nil {
			renderError(ctx, w, r, err)

			return
		}

		writeReplicaJSON(w, info)
	case r.Method == http.MethodPost && op == "blocks":
		var req claimRequest
		if !readReplicaJSON(w, r, &req) {
			return
		}

		if err := checkReplicaID(req.VersionID); err != nil {
			renderAPIError(ctx, w, r, s3err.InvalidArgument, err)

			return
		}

		for _, b := range req.Blocks {
			if err := checkReplicaBlock(b); err != nil {
				renderAPIError(ctx, w, r, s3err.InvalidArgument, err)

				return
			}
		}

		claims, err := rr.ClaimReplicaBlocks(ctx, bucket, req.VersionID, req.Blocks)
		if err != nil {
			renderError(ctx, w, r, err)

			return
		}

		writeReplicaJSON(w, claims)
	case r.Method == http.MethodPut && op == "block":
		h.putReplicaBlock(w, r, rr, bucket)
	case r.Method == http.MethodPost && op == "versions":
		var versions []fs.ReplicaVersion
		if !readReplicaJSON(w, r, &versions) {
			return
		}

		for _, v := range versions {
			if err := checkReplicaID(v.VersionID); err != nil {
				renderAPIError(ctx, w, r, s3err.InvalidArgument, err)

				return
			}

			for _, b := range v.Blocks {
				if err := checkReplicaBlock(b); err != nil {
					renderAPIError(ctx, w, r, s3err.InvalidArgument, err)

					return
				}
			}
		}

		results, err := rr.ApplyReplicas(ctx, bucket, versions)
		if err != nil {
			renderError(ctx, w, r, err)

			return
		}

		writeReplicaJSON(w, results)
	default:
		renderAPIError(ctx, w, r, s3err.InvalidRequest,
			errors.Errorf("%s %s=%q is not a replication request", r.Method, replicaParam, op))
	}
}

func (h *handler) putReplicaBlock(w http.ResponseWriter, r *http.Request, rr fs.ReplicaReceiver, bucket string) {
	ctx := r.Context()
	q := r.URL.Query()

	b := fs.ReplicaBlock{Hash: q.Get("hash"), Scheme: q.Get("scheme")}

	data, err := io.ReadAll(io.LimitReader(r.Body, maxReplicaBlock+1))
	if err != nil {
		renderError(ctx, w, r, errors.Wrap(err, "read block"))

		return
	}

	b.Size = int64(len(data))

	if err := checkReplicaID(q.Get("version")); err != nil {
		renderAPIError(ctx, w, r, s3err.InvalidArgument, err)

		return
	}

	if err := checkReplicaBlock(b); err != nil {
		renderAPIError(ctx, w, r, s3err.InvalidArgument, err)

		return
	}

	if err := rr.PutReplicaBlock(ctx, bucket, q.Get("version"), b, data); err != nil {
		renderError(ctx, w, r, err)

		return
	}

	w.WriteHeader(http.StatusNoContent)
}

// checkReplicaID refuses what cannot be a version's own ID.
func checkReplicaID(id string) error {
	if id == fs.NullVersionID || !fs.ValidVersionID(id) {
		return errors.Errorf("invalid version ID %q", id)
	}

	return nil
}

// checkReplicaBlock refuses a block that cannot be one: not a SHA-256, or
// empty or too large.
func checkReplicaBlock(b fs.ReplicaBlock) error {
	if raw, err := hex.DecodeString(b.Hash); err != nil || len(raw) != 32 {
		return errors.Errorf("invalid block hash %q", b.Hash)
	}

	if b.Size <= 0 || b.Size > maxReplicaBlock {
		return errors.Errorf("block %s: invalid size %d", b.Hash, b.Size)
	}

	return nil
}

func readReplicaJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxReplicaJSON+1))
	if err != nil {
		renderError(r.Context(), w, r, errors.Wrap(err, "read body"))

		return false
	}

	if len(body) > maxReplicaJSON {
		renderAPIError(r.Context(), w, r, s3err.EntityTooLarge, errors.Errorf("body over %d bytes", maxReplicaJSON))

		return false
	}

	if err := json.Unmarshal(body, v); err != nil {
		renderAPIError(r.Context(), w, r, s3err.InvalidRequest, errors.Wrap(err, "decode body"))

		return false
	}

	return true
}

func writeReplicaJSON(w http.ResponseWriter, v any) {
	body, err := json.Marshal(v)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)

		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	_, _ = w.Write(body)
}
