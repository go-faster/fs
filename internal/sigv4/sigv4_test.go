package sigv4

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/stretchr/testify/require"
)

const (
	testAccessKey = "AKIAIOSFODNN7EXAMPLE"
	testSecretKey = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

func testLookup(access string) (string, bool) {
	if access == testAccessKey {
		return testSecretKey, true
	}

	return "", false
}

// signHeader signs req in place with the real aws-sdk-go-v2 signer.
func signHeader(t *testing.T, req *http.Request, payloadHash string, at time.Time) {
	t.Helper()

	creds := aws.Credentials{AccessKeyID: testAccessKey, SecretAccessKey: testSecretKey}
	opt := func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true }
	err := v4.NewSigner().SignHTTP(context.Background(), creds, req, payloadHash, "s3", "us-east-1", at, opt)
	require.NoError(t, err)
}

func newVerifier(at time.Time) *Verifier {
	v := NewVerifier(testLookup)
	v.now = func() time.Time { return at }

	return v
}

func TestVerifyHeader_RoundTrip(t *testing.T) {
	now := time.Now().UTC()

	cases := []struct {
		name        string
		method      string
		target      string
		payloadHash string
		headers     map[string]string
	}{
		{"GetObject", http.MethodGet, "http://s3.local/bucket/key.txt", emptyPayloadHash, nil},
		{"NestedKey", http.MethodGet, "http://s3.local/bucket/a/b/c%20d.txt", emptyPayloadHash, nil},
		{"ListWithQuery", http.MethodGet, "http://s3.local/bucket?list-type=2&prefix=a/b&delimiter=/", emptyPayloadHash, nil},
		{"UnsignedPayloadPut", http.MethodPut, "http://s3.local/bucket/obj", unsignedPayload, map[string]string{"Content-Type": "text/plain"}},
		{"SpecialCharsKey", http.MethodPut, "http://s3.local/bucket/wei%2Brd%20key%21.txt", unsignedPayload, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(tc.method, tc.target, http.NoBody)
			require.NoError(t, err)

			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}

			req.Header.Set("X-Amz-Content-Sha256", tc.payloadHash)
			signHeader(t, req, tc.payloadHash, now)

			res, err := newVerifier(now).Verify(req)
			require.NoError(t, err)
			require.Equal(t, testAccessKey, res.AccessKey)
			require.False(t, res.Streaming)
		})
	}
}

func TestVerifyHeader_WrongSecret(t *testing.T) {
	now := time.Now().UTC()

	req, err := http.NewRequest(http.MethodGet, "http://s3.local/bucket/key", http.NoBody)
	require.NoError(t, err)
	req.Header.Set("X-Amz-Content-Sha256", emptyPayloadHash)
	signHeader(t, req, emptyPayloadHash, now)

	// Verifier resolves the same access key to a different secret.
	v := NewVerifier(func(string) (string, bool) { return "wrong-secret", true })
	v.now = func() time.Time { return now }

	_, err = v.Verify(req)
	require.ErrorIs(t, err, ErrSignatureMismatch)
}

func TestVerifyHeader_UnknownAccessKey(t *testing.T) {
	now := time.Now().UTC()

	req, err := http.NewRequest(http.MethodGet, "http://s3.local/bucket/key", http.NoBody)
	require.NoError(t, err)
	req.Header.Set("X-Amz-Content-Sha256", emptyPayloadHash)
	signHeader(t, req, emptyPayloadHash, now)

	v := NewVerifier(func(string) (string, bool) { return "", false })
	v.now = func() time.Time { return now }

	_, err = v.Verify(req)
	require.ErrorIs(t, err, ErrUnknownAccessKey)
}

func TestVerifyHeader_Tampered(t *testing.T) {
	now := time.Now().UTC()

	req, err := http.NewRequest(http.MethodGet, "http://s3.local/bucket/key", http.NoBody)
	require.NoError(t, err)
	req.Header.Set("X-Amz-Content-Sha256", emptyPayloadHash)
	signHeader(t, req, emptyPayloadHash, now)

	// Tamper with the path after signing.
	req.URL.Path = "/bucket/other-key"

	_, err = newVerifier(now).Verify(req)
	require.ErrorIs(t, err, ErrSignatureMismatch)
}

func TestVerifyHeader_ClockSkew(t *testing.T) {
	signedAt := time.Now().UTC()

	req, err := http.NewRequest(http.MethodGet, "http://s3.local/bucket/key", http.NoBody)
	require.NoError(t, err)
	req.Header.Set("X-Amz-Content-Sha256", emptyPayloadHash)
	signHeader(t, req, emptyPayloadHash, signedAt)

	// Server clock 20 minutes ahead of the signature.
	v := newVerifier(signedAt.Add(20 * time.Minute))

	_, err = v.Verify(req)
	require.ErrorIs(t, err, ErrClockSkew)
}

// presign produces a presigned URL with the given expiry. The raw v4 signer
// signs whatever query is present but does not add X-Amz-Expires itself (the S3
// presign client does), so we set it before signing, exactly as a real
// presigned URL carries it.
func presign(t *testing.T, method, target string, expires time.Duration, at time.Time) string {
	t.Helper()

	req, err := http.NewRequest(method, target, http.NoBody)
	require.NoError(t, err)

	q := req.URL.Query()
	q.Set("X-Amz-Expires", itoa(int64(expires.Seconds())))
	req.URL.RawQuery = q.Encode()

	creds := aws.Credentials{AccessKeyID: testAccessKey, SecretAccessKey: testSecretKey}
	opt := func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true }
	signed, _, err := v4.NewSigner().PresignHTTP(context.Background(), creds, req, unsignedPayload, "s3", "us-east-1", at, opt)
	require.NoError(t, err)

	return signed
}

func TestVerifyPresigned_RoundTrip(t *testing.T) {
	now := time.Now().UTC()

	signed := presign(t, http.MethodGet, "http://s3.local/bucket/key.txt", 15*time.Minute, now)
	require.Contains(t, signed, "X-Amz-Expires=")

	u, err := url.Parse(signed)
	require.NoError(t, err)

	preq, err := http.NewRequest(http.MethodGet, signed, http.NoBody)
	require.NoError(t, err)

	preq.Host = u.Host

	res, err := newVerifier(now).Verify(preq)
	require.NoError(t, err)
	require.Equal(t, testAccessKey, res.AccessKey)
}

func TestVerifyPresigned_Expired(t *testing.T) {
	signedAt := time.Now().UTC()

	signed := presign(t, http.MethodGet, "http://s3.local/bucket/key.txt", 60*time.Second, signedAt)

	preq, err := http.NewRequest(http.MethodGet, signed, http.NoBody)
	require.NoError(t, err)

	u, _ := url.Parse(signed)

	preq.Host = u.Host

	// Verify well past the 60s expiry.
	_, err = newVerifier(signedAt.Add(5 * time.Minute)).Verify(preq)
	require.ErrorIs(t, err, ErrRequestExpired)
}

func TestVerifyPresigned_TooLongExpiry(t *testing.T) {
	now := time.Now().UTC()

	signed := presign(t, http.MethodGet, "http://s3.local/bucket/key.txt", 8*24*time.Hour, now)

	preq, err := http.NewRequest(http.MethodGet, signed, http.NoBody)
	require.NoError(t, err)

	u, _ := url.Parse(signed)

	preq.Host = u.Host

	_, err = newVerifier(now).Verify(preq)
	require.ErrorIs(t, err, ErrMalformedSignature)
}

func TestVerify_MissingSignature(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://s3.local/bucket/key", http.NoBody)
	require.NoError(t, err)

	_, err = newVerifier(time.Now()).Verify(req)
	require.ErrorIs(t, err, ErrMissingSignature)
}

func TestAWSURIEncode(t *testing.T) {
	require.Equal(t, "a/b/c", awsURIEncode("a/b/c", false))
	require.Equal(t, "a%2Fb%2Fc", awsURIEncode("a/b/c", true))
	require.Equal(t, "hello%20world", awsURIEncode("hello world", false))
	require.Equal(t, "a%2Bb", awsURIEncode("a+b", false))
	require.Equal(t, "-_.~", awsURIEncode("-_.~", false))
	require.Equal(t, strings.Repeat("%00", 1), awsURIEncode("\x00", false))
}
