// Package cors provides per-bucket CORS configuration for the S3 server:
// which cross-origin requests are allowed and what preflight responses to
// return. Configuration is supplied at construction (not via the S3
// PutBucketCors subresource); the server answers OPTIONS preflight and adds
// CORS response headers to matching cross-origin requests.
package cors

import "strings"

// Rule allows cross-origin requests matching AllowedOrigins and AllowedMethods.
// A "*" entry in AllowedOrigins or AllowedHeaders matches anything.
type Rule struct {
	AllowedOrigins []string
	AllowedMethods []string
	AllowedHeaders []string
	ExposeHeaders  []string
	MaxAgeSeconds  int
}

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

// AllowsHeaders reports whether the rule permits every requested header (the
// comma-separated Access-Control-Request-Headers value).
func (r *Rule) AllowsHeaders(requested string) bool {
	if requested == "" {
		return true
	}

	for h := range strings.SplitSeq(requested, ",") {
		h = strings.TrimSpace(h)
		if h == "" {
			continue
		}

		if !headerAllowed(r.AllowedHeaders, h) {
			return false
		}
	}

	return true
}

func originAllowed(allowed []string, origin string) bool {
	for _, a := range allowed {
		if a == "*" || strings.EqualFold(a, origin) {
			return true
		}
	}

	return false
}

func headerAllowed(allowed []string, header string) bool {
	for _, a := range allowed {
		if a == "*" || strings.EqualFold(a, header) {
			return true
		}
	}

	return false
}

func contains(ss []string, s string) bool {
	for _, v := range ss {
		if strings.EqualFold(v, s) {
			return true
		}
	}

	return false
}
