package handler_test

import (
	"encoding/xml"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/core/handler"
)

// TestListObjects_MarkerAlwaysPresent pins the empty Marker element on a V1
// listing: SDKs read the field unconditionally, and an omitted element reads as
// a missing key rather than as "no marker".
func TestListObjects_MarkerAlwaysPresent(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a", "", nil).Code)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a/k", "v", nil).Code)

	body := do(t, h, http.MethodGet, "/bucket-a", "", nil).Body.String()
	require.Contains(t, body, "<Marker></Marker>")

	// V2 must not carry Marker at all, and echoes an empty continuation token
	// only when one was sent.
	body = do(t, h, http.MethodGet, "/bucket-a?list-type=2", "", nil).Body.String()
	require.NotContains(t, body, "<Marker>")
	require.NotContains(t, body, "<ContinuationToken>")

	body = do(t, h, http.MethodGet, "/bucket-a?list-type=2&continuation-token=", "", nil).Body.String()
	require.Contains(t, body, "<ContinuationToken></ContinuationToken>")
}

// TestListObjects_AllowUnordered covers the RGW listing extension: accepted on
// its own, refused together with a delimiter.
func TestListObjects_AllowUnordered(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a", "", nil).Code)

	require.Equal(t, http.StatusOK,
		do(t, h, http.MethodGet, "/bucket-a?allow-unordered=true", "", nil).Code)
	require.Equal(t, http.StatusBadRequest,
		do(t, h, http.MethodGet, "/bucket-a?allow-unordered=true&delimiter=/", "", nil).Code)
}

// TestDeleteObjects_KeyLimit pins the 1000-key cap: accepting more would tell a
// client a request it should have split was taken whole.
func TestDeleteObjects_KeyLimit(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a", "", nil).Code)

	var b strings.Builder

	b.WriteString("<Delete>")

	for i := range 1001 {
		b.WriteString("<Object><Key>k-" + strconv.Itoa(i) + "</Key></Object>")
	}

	b.WriteString("</Delete>")

	rec := do(t, h, http.MethodPost, "/bucket-a?delete", b.String(), nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "MalformedXML")
}

// TestGetObject_Torrent covers the legacy subresource: a typed 404 beats
// handing back object bytes that are not a torrent.
func TestGetObject_Torrent(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a", "", nil).Code)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a/obj", "body", nil).Code)

	rec := do(t, h, http.MethodGet, "/bucket-a/obj?torrent", "", nil)
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), "NoSuchKey")
	require.NotContains(t, rec.Body.String(), "body")
}

// TestGetObject_ResponseOverrides covers the response-* query parameters that
// let a presigned URL dictate what the browser does with the object.
func TestGetObject_ResponseOverrides(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a", "", nil).Code)
	require.Equal(t, http.StatusOK,
		do(t, h, http.MethodPut, "/bucket-a/obj", "body", map[string]string{"Content-Type": "text/plain"}).Code)

	rec := do(t, h, http.MethodGet,
		"/bucket-a/obj?response-content-type=foo%2Fbar&response-content-disposition=attachment", "", nil)
	require.Equal(t, "foo/bar", rec.Header().Get("Content-Type"))
	require.Equal(t, "attachment", rec.Header().Get("Content-Disposition"))
}

// TestObjectMetadata_ExpiresAndTagCount covers the two headers a stored object
// reports beyond its own content: the Expires it was written with, and how many
// tags it carries.
func TestObjectMetadata_ExpiresAndTagCount(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a", "", nil).Code)

	expires := "Wed, 21 Oct 2026 07:28:00 GMT"
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a/obj", "body", map[string]string{
		"Expires":       expires,
		"x-amz-tagging": "a=1&b=2",
	}).Code)

	rec := do(t, h, http.MethodHead, "/bucket-a/obj", "", nil)
	require.Equal(t, expires, rec.Header().Get("Expires"))
	require.Equal(t, "2", rec.Header().Get("x-amz-tagging-count"))

	// An untagged object reports no count at all, so "0" is never ambiguous.
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a/plain", "body", nil).Code)
	require.Empty(t, do(t, h, http.MethodHead, "/bucket-a/plain", "", nil).Header().Get("x-amz-tagging-count"))
}

// TestObjectMetadata_NonASCIIRoundTrip pins the encoding of user metadata.
// Header values are read one byte per character by every client, so a value
// echoed back as stored UTF-8 shows up as mojibake.
func TestObjectMetadata_NonASCIIRoundTrip(t *testing.T) {
	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a", "", nil).Code)

	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/bucket-a/obj", "body", map[string]string{
		"x-amz-meta-note": "Hello Worldé",
	}).Code)

	// Scan the map case-insensitively: the header is emitted all-lowercase on
	// purpose, the way AWS emits it, so neither Header.Get nor a canonical-key
	// lookup would find it.
	var got []string

	for name, values := range do(t, h, http.MethodHead, "/bucket-a/obj", "", nil).Header() {
		if strings.EqualFold(name, "x-amz-meta-note") {
			got = values
		}
	}

	require.Equal(t, []string{"Hello World\xe9"}, got)
}

// TestOptions_WithoutOrigin covers the OPTIONS that is not a preflight: 400,
// not the 403 that falling through to auth would produce.
func TestOptions_WithoutOrigin(t *testing.T) {
	h := newStorageHandler(t)

	rec := do(t, h, http.MethodOptions, "/bucket-a/obj", "", nil)
	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "InvalidRequest")
}

// TestListBuckets_Paginated covers max-buckets and the continuation token,
// including the token's absence on the last page.
func TestListBuckets_Paginated(t *testing.T) {
	h := newStorageHandler(t)
	for _, b := range []string{"bucket-a", "bucket-b"} {
		require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+b, "", nil).Code)
	}

	var result handler.ListAllMyBucketsResult

	rec := do(t, h, http.MethodGet, "/?max-buckets=1", "", nil)
	require.NoError(t, xml.NewDecoder(strings.NewReader(rec.Body.String())).Decode(&result))
	require.Len(t, result.Buckets.Buckets, 1)
	require.Equal(t, "bucket-a", result.Buckets.Buckets[0].Name)
	require.NotEmpty(t, result.ContinuationToken)

	next := handler.ListAllMyBucketsResult{}
	rec = do(t, h, http.MethodGet, "/?max-buckets=1&continuation-token="+result.ContinuationToken, "", nil)
	require.NoError(t, xml.NewDecoder(strings.NewReader(rec.Body.String())).Decode(&next))
	require.Len(t, next.Buckets.Buckets, 1)
	require.Equal(t, "bucket-b", next.Buckets.Buckets[0].Name)
	require.Empty(t, next.ContinuationToken)
}
