package handler

import (
	"cmp"
	"net"
	"net/http"
	"slices"
	"strings"

	"github.com/go-faster/fs/internal/sigv4"
)

// WithDomains enables virtual-host-style addressing for these service
// domains: a request to <bucket>.<domain> addresses that bucket, and the path
// is the key. Path-style keeps working on every host. Empty, the default,
// is pure path-style.
func WithDomains(domains []string) Option {
	return func(o *options) { o.domains = Domains(domains) }
}

// Domains normalizes service domains for BucketFromHost: lowercased, without
// a trailing dot, longest first, so that the most specific domain wins.
func Domains(in []string) []string {
	out := make([]string, 0, len(in))

	for _, d := range in {
		if d = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(d)), "."); d != "" {
			out = append(out, d)
		}
	}

	slices.SortFunc(out, func(a, b string) int { return cmp.Compare(len(b), len(a)) })

	return slices.Compact(out)
}

// BucketFromHost returns the bucket a Host header names under domains, as
// Domains normalized them. The port is ignored and case does not matter. A
// host equal to a domain, matching none, or an IP literal names no bucket:
// the request is path-style.
func BucketFromHost(domains []string, host string) (string, bool) {
	if len(domains) == 0 || host == "" {
		return "", false
	}

	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}

	host = strings.TrimSuffix(strings.ToLower(host), ".")

	if net.ParseIP(strings.Trim(host, "[]")) != nil {
		return "", false
	}

	for _, d := range domains {
		if host == d {
			return "", false
		}

		if bucket, ok := strings.CutSuffix(host, "."+d); ok {
			return bucket, bucket != ""
		}
	}

	return "", false
}

// virtualHosts rewrites a request whose Host names a bucket to its
// path-style form, so that every layer after it — CORS, auth, the router and
// the handlers — reads the bucket from the path as it always has. The path
// as sent is kept for SigV4, which verifies the signature over it.
//
// Bucket names are lowercase letters, digits, '-' and '.', none of which
// needs escaping, so the rewrite cannot change how the key decodes.
func virtualHosts(domains []string, next http.Handler) http.Handler {
	if len(domains) == 0 {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bucket, ok := BucketFromHost(domains, r.Host)
		if !ok {
			next.ServeHTTP(w, r)

			return
		}

		ctx := sigv4.WithSignedPath(r.Context(), r.URL.EscapedPath())

		u := *r.URL
		u.Path = "/" + bucket + cmp.Or(u.Path, "/")

		if u.RawPath != "" {
			u.RawPath = "/" + bucket + u.RawPath
		}

		r = r.WithContext(ctx)
		r.URL = &u

		next.ServeHTTP(w, r)
	})
}
