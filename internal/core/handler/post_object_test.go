package handler_test

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/auth"
	"github.com/go-faster/fs/internal/core/handler"
	"github.com/go-faster/fs/internal/core/service"
	"github.com/go-faster/fs/storagemem"
)

// postFields builds a multipart/form-data body with the file part last, which
// is the order S3 requires and the handler relies on.
func postFields(t testing.TB, fields [][2]string, file string) (string, io.Reader) {
	t.Helper()

	var body bytes.Buffer

	w := multipart.NewWriter(&body)

	for _, f := range fields {
		require.NoError(t, w.WriteField(f[0], f[1]))
	}

	part, err := w.CreateFormFile("file", "upload.txt")
	require.NoError(t, err)
	_, err = part.Write([]byte(file))
	require.NoError(t, err)
	require.NoError(t, w.Close())

	return w.FormDataContentType(), &body
}

// postBucket is the bucket every upload in this file targets.
const postBucket = "bucket-a"

// doPost issues a form upload and returns the recorder.
func doPost(t testing.TB, h http.Handler, fields [][2]string, file string) *httptest.ResponseRecorder {
	t.Helper()

	contentType, body := postFields(t, fields, file)

	req := httptest.NewRequest(http.MethodPost, "/"+postBucket, body)
	req.Header.Set("Content-Type", contentType)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}

// encodePolicy renders a policy document the way a form carries it.
func encodePolicy(t testing.TB, conditions []any) string {
	t.Helper()

	doc, err := json.Marshal(map[string]any{
		"expiration": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		"conditions": conditions,
	})
	require.NoError(t, err)

	return base64.StdEncoding.EncodeToString(doc)
}

// TestPostObject_Anonymous covers the plain browser upload against a bucket
// that accepts anonymous writes: no policy, no signature, 204 and the object
// lands.
func TestPostObject_Anonymous(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK,
		do(t, h, http.MethodPut, "/bucket-a", "", map[string]string{"x-amz-acl": "public-read-write"}).Code)

	rec := doPost(t, h, [][2]string{
		{"key", "foo.txt"},
		{"Content-Type", "text/plain"},
	}, "bar")
	require.Equal(t, http.StatusNoContent, rec.Code)

	got := do(t, h, http.MethodGet, "/bucket-a/foo.txt", "", nil)
	require.Equal(t, "bar", got.Body.String())
	require.Equal(t, "text/plain", got.Header().Get("Content-Type"))
}

// TestPostObject_SuccessActionStatus covers the status a form may ask for:
// 200 and 201 are honored, anything else falls back to the default 204.
func TestPostObject_SuccessActionStatus(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK,
		do(t, h, http.MethodPut, "/bucket-a", "", map[string]string{"x-amz-acl": "public-read-write"}).Code)

	rec := doPost(t, h, [][2]string{
		{"key", "created.txt"}, {"success_action_status", "201"},
	}, "bar")
	require.Equal(t, http.StatusCreated, rec.Code)
	require.Contains(t, rec.Body.String(), "<Key>created.txt</Key>")

	rec = doPost(t, h, [][2]string{
		{"key", "odd.txt"}, {"success_action_status", "404"},
	}, "bar")
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Empty(t, rec.Body.String())
}

// TestPostObject_KeyFromFilename covers ${filename}, which is how a form that
// cannot know the name in advance names the object.
func TestPostObject_KeyFromFilename(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK,
		do(t, h, http.MethodPut, "/bucket-a", "", map[string]string{"x-amz-acl": "public-read-write"}).Code)

	rec := doPost(t, h, [][2]string{{"key", "${filename}"}}, "bar")
	require.Equal(t, http.StatusNoContent, rec.Code)

	require.Equal(t, "bar", do(t, h, http.MethodGet, "/bucket-a/upload.txt", "", nil).Body.String())
}

// TestPostObject_AnonymousDeniedOnPrivateBucket pins the permission a form
// upload without a policy actually needs. It runs against an *authenticated*
// handler, because on a server with no credentials there is nothing to deny —
// every request is anonymous there, and the upload is accepted like any other
// write.
func TestPostObject_AnonymousDeniedOnPrivateBucket(t *testing.T) {
	store, err := auth.NewStore(auth.Config{
		Keys: []auth.Key{{
			AccessKey: "AKIAIOSFODNN7EXAMPLE",
			SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			Grants:    []auth.Grant{{Pattern: "*", Permission: auth.Admin}},
		}},
	})
	require.NoError(t, err)

	backend := storagemem.New()
	require.NoError(t, backend.CreateBucket(t.Context(), "bucket-a"))

	h := handler.New(service.New(backend), handler.WithAuthenticator(store))

	rec := doPost(t, h, [][2]string{{"key", "foo.txt"}}, "bar")
	require.Equal(t, http.StatusForbidden, rec.Code)

	// The same upload against a bucket that accepts anonymous writes lands.
	require.NoError(t, backend.SetBucketACL(t.Context(), "bucket-a", fs.ACLPublicReadWrite))

	rec = doPost(t, h, [][2]string{{"key", "foo.txt"}}, "bar")
	require.Equal(t, http.StatusNoContent, rec.Code)
}

// TestPostObject_MalformedPolicy covers the split between a policy that is not
// a policy (400) and one that parses and refuses the upload (403). Getting this
// backwards tells a caller with a bug that they need a permission.
func TestPostObject_MalformedPolicy(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK,
		do(t, h, http.MethodPut, "/bucket-a", "", map[string]string{"x-amz-acl": "public-read-write"}).Code)

	malformed := []struct {
		name   string
		policy string
	}{
		{"not base64", "!!!!"},
		{"no conditions", base64.StdEncoding.EncodeToString([]byte(
			`{"expiration":"2100-01-01T00:00:00Z"}`))},
		{"no expiration", base64.StdEncoding.EncodeToString([]byte(
			`{"conditions":[]}`))},
		{"wrong case", base64.StdEncoding.EncodeToString([]byte(
			`{"EXPIRATION":"2100-01-01T00:00:00Z","CONDITIONS":[]}`))},
		{"empty condition", base64.StdEncoding.EncodeToString([]byte(
			`{"expiration":"2100-01-01T00:00:00Z","conditions":[{}]}`))},
	}

	for _, tt := range malformed {
		t.Run(tt.name, func(t *testing.T) {
			rec := doPost(t, h, [][2]string{
				{"key", "foo.txt"}, {"policy", tt.policy},
				{"AWSAccessKeyId", "key"}, {"signature", "sig"},
			}, "bar")

			// "not base64" is the one shape that is a denial rather than a
			// malformed document: nothing about it can be read at all.
			if tt.name == "not base64" {
				require.Equal(t, http.StatusForbidden, rec.Code)
				return
			}

			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		})
	}
}

// TestPostObject_ExpiredPolicy covers the expiration, which is the only thing
// standing between a leaked form and an unbounded upload window.
func TestPostObject_ExpiredPolicy(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK,
		do(t, h, http.MethodPut, "/bucket-a", "", map[string]string{"x-amz-acl": "public-read-write"}).Code)

	doc, err := json.Marshal(map[string]any{
		"expiration": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
		"conditions": []any{map[string]string{"bucket": "bucket-a"}},
	})
	require.NoError(t, err)

	rec := doPost(t, h, [][2]string{
		{"key", "foo.txt"},
		{"policy", base64.StdEncoding.EncodeToString(doc)},
		{"AWSAccessKeyId", "key"}, {"signature", "sig"},
	}, "bar")
	require.Equal(t, http.StatusForbidden, rec.Code)
}

// TestPostObject_UncoveredField pins the exhaustiveness of a policy: a field
// the form sends and the policy does not mention was never authorized.
func TestPostObject_UncoveredField(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK,
		do(t, h, http.MethodPut, "/bucket-a", "", map[string]string{"x-amz-acl": "public-read-write"}).Code)

	policy := encodePolicy(t, []any{
		map[string]string{"bucket": "bucket-a"},
		[]any{"starts-with", "$key", "foo"},
	})

	rec := doPost(t, h, [][2]string{
		{"key", "foo.txt"},
		{"Content-Type", "text/plain"}, // not mentioned by the policy
		{"policy", policy},
		{"AWSAccessKeyId", "key"}, {"signature", "sig"},
	}, "bar")
	require.Equal(t, http.StatusForbidden, rec.Code)

	// An x-ignore-* field needs no condition: S3 reserves it for the caller.
	policy = encodePolicy(t, []any{
		map[string]string{"bucket": "bucket-a"},
		[]any{"starts-with", "$key", "foo"},
	})

	rec = doPost(t, h, [][2]string{
		{"key", "foo.txt"},
		{"x-ignore-note", "anything"},
		{"policy", policy},
		{"AWSAccessKeyId", "key"}, {"signature", "sig"},
	}, "bar")
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

// TestPostObject_ContentLengthRange covers the size bounds, which are what
// keeps a public upload form from being an unbounded disk.
func TestPostObject_ContentLengthRange(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK,
		do(t, h, http.MethodPut, "/bucket-a", "", map[string]string{"x-amz-acl": "public-read-write"}).Code)

	policy := encodePolicy(t, []any{
		map[string]string{"bucket": "bucket-a"},
		[]any{"starts-with", "$key", "foo"},
		[]any{"content-length-range", 2, 4},
	})

	fields := func() [][2]string {
		return [][2]string{
			{"key", "foo.txt"}, {"policy", policy},
			{"AWSAccessKeyId", "key"}, {"signature", "sig"},
		}
	}

	require.Equal(t, http.StatusNoContent, doPost(t, h, fields(), "abc").Code)

	rec := doPost(t, h, fields(), "abcdefgh")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "EntityTooLarge")

	rec = doPost(t, h, fields(), "a")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "EntityTooSmall")

	// The undersized upload left nothing behind.
	require.Equal(t, http.StatusOK, do(t, h, http.MethodGet, "/bucket-a/foo.txt", "", nil).Code)
	require.Equal(t, "abc", do(t, h, http.MethodGet, "/bucket-a/foo.txt", "", nil).Body.String())
}

// TestPostObject_RedirectNeedsPolicy pins the open-redirect guard: a form with
// no policy cannot make the endpoint redirect anywhere.
func TestPostObject_RedirectNeedsPolicy(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK,
		do(t, h, http.MethodPut, "/bucket-a", "", map[string]string{"x-amz-acl": "public-read-write"}).Code)

	rec := doPost(t, h, [][2]string{
		{"key", "foo.txt"},
		{"success_action_redirect", "https://evil.example/"},
	}, "bar")
	require.Equal(t, http.StatusNoContent, rec.Code)
	require.Empty(t, rec.Header().Get("Location"))

	policy := encodePolicy(t, []any{
		map[string]string{"bucket": "bucket-a"},
		[]any{"starts-with", "$key", "foo"},
		[]any{"eq", "$success_action_redirect", "https://example.test/done"},
	})

	rec = doPost(t, h, [][2]string{
		{"key", "foo.txt"},
		{"success_action_redirect", "https://example.test/done"},
		{"policy", policy},
		{"AWSAccessKeyId", "key"}, {"signature", "sig"},
	}, "bar")
	require.Equal(t, http.StatusSeeOther, rec.Code)

	// bucket, key, etag — in that order, which is what S3 emits and what
	// clients compare against.
	require.Contains(t, rec.Header().Get("Location"),
		fmt.Sprintf("bucket=%s&key=%s&etag=", "bucket-a", "foo.txt"))
}
