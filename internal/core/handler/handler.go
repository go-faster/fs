package handler

import (
	"net/http"
	"strings"

	"go.uber.org/zap"

	"github.com/go-faster/sdk/zctx"

	"github.com/go-faster/fs"
)

type handler struct {
	service fs.Service
}

func New(s fs.Service) http.Handler {
	mux := http.NewServeMux()
	h := handler{
		service: s,
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		path := strings.TrimPrefix(r.URL.Path, "/")
		if path == "" {
			// Root path - list buckets.
			switch r.Method {
			case http.MethodGet:
				h.ListBuckets(w, r)
			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
			}

			return
		}

		// Parse bucket and key from path
		bucket, key, _ := strings.Cut(path, "/")
		zctx.From(ctx).Debug("Parsed bucket and key from path",
			zap.String("bucket", bucket),
			zap.String("key", key),
		)

		if key == "" {
			// Bucket operation only.
			switch r.Method {
			case http.MethodGet:
				h.ListObjects(w, r)
			case http.MethodPut:
				h.CreateBucket(w, r)
			default:
				w.WriteHeader(http.StatusMethodNotAllowed)
			}

			return
		}

		// Object operation.
		switch r.Method {
		case http.MethodGet:
			h.GetObject(w, r)
		case http.MethodPut:
			h.PutObject(w, r)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	return mux
}
