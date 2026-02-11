package handler

import (
	"net/http"
	"strings"

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
		_, key, _ := strings.Cut(path, "/")

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
		case http.MethodPut:
			h.PutObject(w, r)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})

	return mux
}
