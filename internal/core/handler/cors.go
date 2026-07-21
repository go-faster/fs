package handler

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/go-faster/fs/cors"
)

// CORSResolver returns the CORS rules that apply to a bucket.
type CORSResolver interface {
	Rules(bucket string) []cors.Rule
}

// corsMiddleware answers CORS preflight (OPTIONS with an Origin) and adds CORS
// response headers to matching cross-origin requests. It sits outside auth so
// preflight — which carries no credentials — is answered without a 403, and so
// the headers are present on every response the browser sees.
func corsMiddleware(resolver CORSResolver, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			// Not a cross-origin request; nothing to do.
			next.ServeHTTP(w, r)
			return
		}

		bucket, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
		rules := resolver.Rules(bucket)

		if r.Method == http.MethodOptions {
			handlePreflight(w, r, rules, origin)
			return
		}

		if rule := cors.Match(rules, origin, r.Method); rule != nil {
			writeCORSHeaders(w, rule, origin)
		}

		next.ServeHTTP(w, r)
	})
}

// handlePreflight answers an OPTIONS preflight request.
func handlePreflight(w http.ResponseWriter, r *http.Request, rules []cors.Rule, origin string) {
	reqMethod := r.Header.Get("Access-Control-Request-Method")

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
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Add("Vary", "Origin")

	if len(rule.ExposeHeaders) > 0 {
		w.Header().Set("Access-Control-Expose-Headers", strings.Join(rule.ExposeHeaders, ", "))
	}
}
