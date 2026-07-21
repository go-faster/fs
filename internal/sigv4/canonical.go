package sigv4

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
)

// awsURIEncode percent-encodes s per the AWS SigV4 rules: the unreserved set
// (A-Z a-z 0-9 - _ . ~) is left as-is, every other byte becomes %XX with
// uppercase hex. When encodeSlash is false, '/' is also left as-is (used for
// the canonical URI path; S3 encodes path segments but keeps the separators).
func awsURIEncode(s string, encodeSlash bool) string {
	const upperhex = "0123456789ABCDEF"

	var b strings.Builder

	b.Grow(len(s))

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperhex[c>>4])
			b.WriteByte(upperhex[c&0xF])
		}
	}

	return b.String()
}

// canonicalURI returns the canonical URI for an S3 SigV4 request: the escaped
// request path exactly as the client sent (and signed) it. S3 signs the
// wire-encoded path with single encoding and without normalization, which is
// precisely what EscapedPath reconstructs from the request line (RawPath),
// preserving the client's exact encoding — including any encoded slashes in a
// key. An empty path canonicalizes to "/".
func canonicalURI(r *http.Request) string {
	path := r.URL.EscapedPath()
	if path == "" {
		return "/"
	}

	return path
}

// canonicalQuery builds the canonical query string: every parameter (except any
// listed in exclude, e.g. X-Amz-Signature) with key and value URI-encoded and
// the whole set sorted by encoded key then encoded value, joined as k=v pairs.
func canonicalQuery(r *http.Request, exclude ...string) string {
	skip := make(map[string]struct{}, len(exclude))
	for _, e := range exclude {
		skip[e] = struct{}{}
	}

	type kv struct{ k, v string }

	var pairs []kv

	for key, values := range r.URL.Query() {
		if _, ok := skip[key]; ok {
			continue
		}

		ek := awsURIEncode(key, true)
		for _, v := range values {
			pairs = append(pairs, kv{ek, awsURIEncode(v, true)})
		}
	}

	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].k != pairs[j].k {
			return pairs[i].k < pairs[j].k
		}

		return pairs[i].v < pairs[j].v
	})

	var b strings.Builder

	for i, p := range pairs {
		if i > 0 {
			b.WriteByte('&')
		}

		b.WriteString(p.k)
		b.WriteByte('=')
		b.WriteString(p.v)
	}

	return b.String()
}

// canonicalHeaders builds the canonical headers block and confirms every signed
// header is present. Header names are lowercased, values trimmed with internal
// runs of whitespace collapsed to a single space, and multiple values joined
// with ",". The "host" pseudo-header is taken from r.Host.
func canonicalHeaders(r *http.Request, signed []string) (string, bool) {
	var b strings.Builder

	for _, name := range signed {
		lower := strings.ToLower(name)

		var value string

		switch lower {
		case "host":
			value = r.Host
		case "content-length":
			// net/http exposes Content-Length as a field, not a header.
			if v := r.Header.Get("Content-Length"); v != "" {
				value = v
			} else if r.ContentLength >= 0 {
				value = itoa(r.ContentLength)
			}
		default:
			values, ok := r.Header[http.CanonicalHeaderKey(name)]
			if !ok {
				return "", false
			}

			value = strings.Join(values, ",")
		}

		b.WriteString(lower)
		b.WriteByte(':')
		b.WriteString(trimAll(value))
		b.WriteByte('\n')
	}

	return b.String(), true
}

// trimAll trims leading/trailing whitespace and collapses internal whitespace
// runs to a single space, matching the SigV4 header-value normalization.
func trimAll(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// canonicalRequest assembles the canonical request string and returns its
// hex-encoded SHA-256.
func canonicalRequest(method, uri, query, headers, signedHeaders, payloadHash string) string {
	cr := strings.Join([]string{
		method,
		uri,
		query,
		headers,
		signedHeaders,
		payloadHash,
	}, "\n")

	sum := sha256.Sum256([]byte(cr))

	return hex.EncodeToString(sum[:])
}
