// Package cors provides per-bucket CORS configuration for the S3 server:
// which cross-origin requests are allowed and what preflight responses to
// return. Configuration is supplied at construction (not via the S3
// PutBucketCors subresource); the server answers OPTIONS preflight and adds
// CORS response headers to matching cross-origin requests.
package cors

import (
	"strings"

	"github.com/go-faster/fs"
)

// Rule allows cross-origin requests matching AllowedOrigins and AllowedMethods.
// A "*" entry in AllowedOrigins or AllowedHeaders matches anything, and an
// origin pattern may contain one "*" standing for any run of characters.
//
// It is an alias for the domain type: a bucket's rules are stored with the
// bucket, so storage has to name them too, and one type avoids converting at
// every seam.
type Rule = fs.CORSRule

// Config is a per-bucket CORS configuration with an optional default applied to
// buckets without a specific entry.
type Config struct {
	Buckets map[string][]Rule
	Default []Rule
}

// Rules returns the CORS rules that apply to bucket.
func (c Config) Rules(bucket string) []Rule {
	if r, ok := c.Buckets[bucket]; ok {
		return r
	}

	return c.Default
}

// Match returns the first rule allowing origin with method, or nil.
func Match(rules []Rule, origin, method string) *Rule {
	for i := range rules {
		r := &rules[i]
		if originAllowed(r.AllowedOrigins, origin) && contains(r.AllowedMethods, method) {
			return r
		}
	}

	return nil
}

func originAllowed(allowed []string, origin string) bool {
	for _, a := range allowed {
		if originPatternMatches(a, origin) {
			return true
		}
	}

	return false
}

// originPatternMatches reports whether an AllowedOrigin pattern covers origin.
//
// S3 allows a single "*" anywhere in the pattern, standing for any run of
// characters, and the rest is anchored: "*suffix" matches only origins ending
// in "suffix", so "foo.suffix.get" does not match. Anchoring is the whole
// point — an unanchored match would let "evil.com/foo.suffix" through.
func originPatternMatches(pattern, origin string) bool {
	if pattern == "*" {
		return true
	}

	prefix, suffix, wildcard := strings.Cut(pattern, "*")
	if !wildcard {
		return strings.EqualFold(pattern, origin)
	}

	if len(origin) < len(prefix)+len(suffix) {
		return false
	}

	return strings.EqualFold(origin[:len(prefix)], prefix) &&
		strings.EqualFold(origin[len(origin)-len(suffix):], suffix)
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if strings.EqualFold(v, s) {
			return true
		}
	}

	return false
}
