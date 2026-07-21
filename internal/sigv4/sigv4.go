// Package sigv4 verifies AWS Signature Version 4 on incoming S3 requests:
// Authorization-header auth, presigned-URL (query) auth, and the seed signature
// for streaming (aws-chunked) uploads. It recomputes the signature the client
// should have produced from a looked-up secret key and compares in constant
// time; it never signs outgoing requests.
package sigv4

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-faster/errors"
)

const (
	algorithm  = "AWS4-HMAC-SHA256"
	terminator = "aws4_request"

	unsignedPayload          = "UNSIGNED-PAYLOAD"
	streamingPayload         = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	streamingUnsignedTrailer = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
	emptyPayloadHash         = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

	amzTimeFormat = "20060102T150405Z"

	// maxClockSkew bounds how far a header-auth request's timestamp may drift
	// from the server clock.
	maxClockSkew = 15 * time.Minute
	// maxPresignExpiry is the S3 cap on presigned-URL validity (7 days).
	maxPresignExpiry = 7 * 24 * time.Hour
)

// Verification failures. The middleware maps these to S3 error codes.
var (
	// ErrMissingSignature reports a request that carries no SigV4 credentials
	// (neither an AWS4-HMAC-SHA256 Authorization header nor query signature).
	ErrMissingSignature = errors.New("missing signature")
	// ErrMalformedSignature reports a syntactically invalid Authorization
	// header or presigned query string.
	ErrMalformedSignature = errors.New("malformed authorization")
	// ErrUnknownAccessKey reports a credential whose access key is not known.
	ErrUnknownAccessKey = errors.New("unknown access key")
	// ErrSignatureMismatch reports a recomputed signature that does not match.
	ErrSignatureMismatch = errors.New("signature mismatch")
	// ErrRequestExpired reports a presigned URL used past its expiry.
	ErrRequestExpired = errors.New("request expired")
	// ErrClockSkew reports a header-auth timestamp outside the allowed skew.
	ErrClockSkew = errors.New("request time too skewed")
)

// SecretKeyFunc resolves an access key to its secret key. ok is false when the
// access key is unknown.
type SecretKeyFunc func(accessKey string) (secretKey string, ok bool)

// Result is the outcome of a successful verification.
type Result struct {
	// AccessKey is the verified caller's access key.
	AccessKey string
	// Streaming is true when the payload uses aws-chunked framing.
	Streaming bool
	// signedChunks is true only for STREAMING-AWS4-HMAC-SHA256-PAYLOAD, whose
	// chunks carry their own signatures; the unsigned-trailer variant does not.
	signedChunks bool
	// seedSignature and signingKey/scope let the streaming reader verify each
	// chunk signature. Unset for non-streaming requests.
	seedSignature string
	signingKey    []byte
	scope         string
	amzTime       time.Time
}

// credential is the parsed X-Amz-Credential / Authorization credential scope:
// "<accessKey>/<date>/<region>/<service>/aws4_request".
type credential struct {
	accessKey string
	date      string
	region    string
	service   string
}

func (c credential) scope() string {
	return strings.Join([]string{c.date, c.region, c.service, terminator}, "/")
}

func parseCredential(s string) (credential, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 5 || parts[4] != terminator {
		return credential{}, errors.Wrap(ErrMalformedSignature, "credential scope")
	}

	return credential{
		accessKey: parts[0],
		date:      parts[1],
		region:    parts[2],
		service:   parts[3],
	}, nil
}

// Verifier verifies SigV4 on requests using secret keys from lookup. now is
// injectable for tests; nil uses time.Now.
type Verifier struct {
	lookup SecretKeyFunc
	now    func() time.Time
}

// NewVerifier returns a Verifier resolving secrets through lookup.
func NewVerifier(lookup SecretKeyFunc) *Verifier {
	return &Verifier{lookup: lookup}
}

func (v *Verifier) clock() time.Time {
	if v.now != nil {
		return v.now()
	}

	return time.Now()
}

// Verify authenticates r, dispatching to presigned-query or header
// verification. It reads only headers and the URL — never the body — so the
// caller streams the payload normally (streaming chunk signatures are checked
// separately via WrapStreaming).
func (v *Verifier) Verify(r *http.Request) (*Result, error) {
	if r.URL.Query().Get("X-Amz-Algorithm") != "" {
		return v.verifyPresigned(r)
	}

	if strings.HasPrefix(r.Header.Get("Authorization"), algorithm) {
		return v.verifyHeader(r)
	}

	return nil, ErrMissingSignature
}

// verifyHeader verifies Authorization-header auth.
func (v *Verifier) verifyHeader(r *http.Request) (*Result, error) {
	cred, signedHeaders, providedSig, err := parseAuthorization(r.Header.Get("Authorization"))
	if err != nil {
		return nil, err
	}

	amzTime, err := requestTime(r)
	if err != nil {
		return nil, err
	}

	if skew := v.clock().Sub(amzTime); skew > maxClockSkew || skew < -maxClockSkew {
		return nil, errors.Wrap(ErrClockSkew, "header auth")
	}

	secret, ok := v.lookup(cred.accessKey)
	if !ok {
		return nil, errors.Wrapf(ErrUnknownAccessKey, "access key %q", cred.accessKey)
	}

	payloadHash := payloadHashHeader(r)

	headers, ok := canonicalHeaders(r, signedHeaders)
	if !ok {
		return nil, errors.Wrap(ErrMalformedSignature, "signed header absent")
	}

	signedList := strings.Join(lowerAll(signedHeaders), ";")
	crHash := canonicalRequest(r.Method, canonicalURI(r), canonicalQuery(r), headers, signedList, payloadHash)

	signingKey := deriveSigningKey(secret, cred)
	sts := stringToSign(amzTime, cred.scope(), crHash)
	expected := hexHMAC(signingKey, sts)

	if !constantTimeEqual(expected, providedSig) {
		return nil, ErrSignatureMismatch
	}

	res := &Result{AccessKey: cred.accessKey, amzTime: amzTime}

	if isStreaming(payloadHash) {
		res.Streaming = true
		res.signedChunks = payloadHash == streamingPayload
		res.seedSignature = expected
		res.signingKey = signingKey
		res.scope = cred.scope()
	}

	return res, nil
}

// verifyPresigned verifies presigned-URL (query) auth.
func (v *Verifier) verifyPresigned(r *http.Request) (*Result, error) {
	q := r.URL.Query()

	if q.Get("X-Amz-Algorithm") != algorithm {
		return nil, errors.Wrap(ErrMalformedSignature, "unsupported algorithm")
	}

	cred, err := parseCredential(q.Get("X-Amz-Credential"))
	if err != nil {
		return nil, err
	}

	amzTime, err := time.Parse(amzTimeFormat, q.Get("X-Amz-Date"))
	if err != nil {
		return nil, errors.Wrap(ErrMalformedSignature, "X-Amz-Date")
	}

	expires, err := strconv.Atoi(q.Get("X-Amz-Expires"))
	if err != nil || expires <= 0 || time.Duration(expires)*time.Second > maxPresignExpiry {
		return nil, errors.Wrap(ErrMalformedSignature, "X-Amz-Expires")
	}

	if v.clock().After(amzTime.Add(time.Duration(expires) * time.Second)) {
		return nil, ErrRequestExpired
	}

	providedSig := q.Get("X-Amz-Signature")
	if providedSig == "" {
		return nil, errors.Wrap(ErrMalformedSignature, "X-Amz-Signature")
	}

	secret, ok := v.lookup(cred.accessKey)
	if !ok {
		return nil, errors.Wrapf(ErrUnknownAccessKey, "access key %q", cred.accessKey)
	}

	signedHeaders := strings.Split(q.Get("X-Amz-SignedHeaders"), ";")

	headers, ok := canonicalHeaders(r, signedHeaders)
	if !ok {
		return nil, errors.Wrap(ErrMalformedSignature, "signed header absent")
	}

	// Presigned URLs sign an UNSIGNED-PAYLOAD and exclude the signature itself
	// from the canonical query.
	signedList := strings.Join(lowerAll(signedHeaders), ";")
	crHash := canonicalRequest(r.Method, canonicalURI(r), canonicalQuery(r, "X-Amz-Signature"),
		headers, signedList, unsignedPayload)

	signingKey := deriveSigningKey(secret, cred)
	sts := stringToSign(amzTime, cred.scope(), crHash)
	expected := hexHMAC(signingKey, sts)

	if !constantTimeEqual(expected, providedSig) {
		return nil, ErrSignatureMismatch
	}

	return &Result{AccessKey: cred.accessKey, amzTime: amzTime}, nil
}

// parseAuthorization parses an "AWS4-HMAC-SHA256 Credential=…, SignedHeaders=…,
// Signature=…" header.
func parseAuthorization(header string) (cred credential, signedHeaders []string, signature string, err error) {
	rest := strings.TrimPrefix(header, algorithm)
	rest = strings.TrimSpace(rest)

	var credStr, signedStr string

	for part := range strings.SplitSeq(rest, ",") {
		part = strings.TrimSpace(part)
		key, val, ok := strings.Cut(part, "=")

		if !ok {
			return credential{}, nil, "", errors.Wrap(ErrMalformedSignature, "authorization part")
		}

		switch key {
		case "Credential":
			credStr = val
		case "SignedHeaders":
			signedStr = val
		case "Signature":
			signature = val
		}
	}

	if credStr == "" || signedStr == "" || signature == "" {
		return credential{}, nil, "", errors.Wrap(ErrMalformedSignature, "authorization fields")
	}

	cred, err = parseCredential(credStr)
	if err != nil {
		return credential{}, nil, "", err
	}

	return cred, strings.Split(signedStr, ";"), signature, nil
}

// requestTime returns the request timestamp from X-Amz-Date (preferred) or the
// Date header.
func requestTime(r *http.Request) (time.Time, error) {
	if v := r.Header.Get("X-Amz-Date"); v != "" {
		t, err := time.Parse(amzTimeFormat, v)
		if err != nil {
			return time.Time{}, errors.Wrap(ErrMalformedSignature, "X-Amz-Date")
		}

		return t, nil
	}

	if v := r.Header.Get("Date"); v != "" {
		if t, err := http.ParseTime(v); err == nil {
			return t, nil
		}
	}

	return time.Time{}, errors.Wrap(ErrMalformedSignature, "missing date")
}

// payloadHashHeader returns the value used as the canonical request's payload
// hash for header auth: the x-amz-content-sha256 header, defaulting to the
// empty-body hash when absent.
func payloadHashHeader(r *http.Request) string {
	if v := r.Header.Get("X-Amz-Content-Sha256"); v != "" {
		return v
	}

	return emptyPayloadHash
}

func isStreaming(payloadHash string) bool {
	return payloadHash == streamingPayload || payloadHash == streamingUnsignedTrailer
}

// deriveSigningKey computes the SigV4 signing key for a credential scope.
func deriveSigningKey(secret string, cred credential) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), cred.date)
	kRegion := hmacSHA256(kDate, cred.region)
	kService := hmacSHA256(kRegion, cred.service)

	return hmacSHA256(kService, terminator)
}

// stringToSign builds the SigV4 string-to-sign.
func stringToSign(t time.Time, scope, canonicalRequestHash string) string {
	return strings.Join([]string{
		algorithm,
		t.UTC().Format(amzTimeFormat),
		scope,
		canonicalRequestHash,
	}, "\n")
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))

	return h.Sum(nil)
}

func hexHMAC(key []byte, data string) string {
	return hex.EncodeToString(hmacSHA256(key, data))
}

func constantTimeEqual(a, b string) bool {
	return hmac.Equal([]byte(a), []byte(b))
}

func lowerAll(ss []string) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = strings.ToLower(s)
	}

	return out
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
