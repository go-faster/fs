package handler

import (
	"context"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/cors"
	"github.com/go-faster/fs/internal/s3err"
)

// CORSResolver returns the CORS rules that apply to a bucket.
type CORSResolver interface {
	Rules(bucket string) []cors.Rule
}

// corsMiddleware answers CORS preflight (OPTIONS with an Origin) and adds CORS
// response headers to matching cross-origin requests. It sits outside auth so
// preflight — which carries no credentials — is answered without a 403, and so
// the headers are present on every response the browser sees.
func corsMiddleware(resolver CORSResolver, store fs.Storage, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			// Not a cross-origin request; nothing to do.
			next.ServeHTTP(w, r)
			return
		}

		bucket, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		rules := bucketCORSRules(r.Context(), store, resolver, bucket)

		if r.Method == http.MethodOptions {
			handlePreflight(w, r, rules, origin)
			return
		}

		// A cross-origin request that names the method it is really asking
		// about is answered about *that* method: the browser sends
		// Access-Control-Request-Method, and S3 matches on it rather than on
		// the verb carrying the header.
		method := r.Header.Get("Access-Control-Request-Method")
		if method == "" {
			method = r.Method
		}

		if rule := cors.Match(rules, origin, method); rule != nil {
			writeCORSHeaders(w, rule, origin)
			w.Header().Set("Access-Control-Allow-Methods", method)
		}

		next.ServeHTTP(w, r)
	})
}

// bucketCORSRules resolves the rules that apply to a bucket: the ones set
// through the ?cors subresource when it has any, otherwise whatever the
// deployment configured out of band. A bucket that was configured over the API
// must win — that call is the more specific statement of intent.
func bucketCORSRules(ctx context.Context, store fs.Storage, resolver CORSResolver, bucket string) []cors.Rule {
	if bucket != "" {
		if s, ok := store.(fs.BucketCORSStore); ok {
			if rules, err := s.BucketCORS(ctx, bucket); err == nil && len(rules) > 0 {
				return rules
			}
		}
	}

	if resolver == nil {
		return nil
	}

	return resolver.Rules(bucket)
}

// handlePreflight answers an OPTIONS preflight request.
//
// A preflight must say which method it is asking about; without that there is
// nothing to decide, and S3 rejects the request as malformed rather than
// denying it.
func handlePreflight(w http.ResponseWriter, r *http.Request, rules []cors.Rule, origin string) {
	reqMethod := r.Header.Get("Access-Control-Request-Method")
	if reqMethod == "" {
		s3err.WriteAPI(w, r, s3err.MissingRequestMethod)
		return
	}

	rule := cors.Match(rules, origin, reqMethod)
	if rule == nil || !rule.AllowsHeaders(r.Header.Get("Access-Control-Request-Headers")) {
		// Preflight not allowed: respond 403 without CORS headers so the
		// browser blocks the request.
		w.WriteHeader(http.StatusForbidden)
		return
	}

	writeCORSHeaders(w, rule, origin)
	w.Header().Set("Access-Control-Allow-Methods", strings.Join(rule.AllowedMethods, ", "))

	if reqHeaders := r.Header.Get("Access-Control-Request-Headers"); reqHeaders != "" {
		w.Header().Set("Access-Control-Allow-Headers", reqHeaders)
	}

	if rule.MaxAgeSeconds > 0 {
		w.Header().Set("Access-Control-Max-Age", strconv.Itoa(rule.MaxAgeSeconds))
	}

	w.WriteHeader(http.StatusOK)
}

// writeCORSHeaders sets the Allow-Origin / Expose-Headers / Vary headers common
// to preflight and actual responses.
func writeCORSHeaders(w http.ResponseWriter, rule *cors.Rule, origin string) {
	// A rule that allows every origin is reported as "*", not as the caller's
	// own origin: the answer does not depend on who asked, and echoing the
	// origin would tell a cache otherwise.
	allow := origin

	for _, a := range rule.AllowedOrigins {
		if a == "*" {
			allow = "*"
			break
		}
	}

	w.Header().Set("Access-Control-Allow-Origin", allow)
	w.Header().Add("Vary", "Origin")

	if len(rule.ExposeHeaders) > 0 {
		w.Header().Set("Access-Control-Expose-Headers", strings.Join(rule.ExposeHeaders, ", "))
	}
}
