package handler_test

import (
	"encoding/xml"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/core/handler"
	"github.com/go-faster/fs/internal/core/service"
	"github.com/go-faster/fs/storagemem"
)

func TestBucketFromHost(t *testing.T) {
	domains := handler.Domains([]string{"Example.com.", "s3.example.com", "s3.internal"})

	for _, tc := range []struct {
		host, bucket string
		ok           bool
	}{
		{host: "photos.s3.example.com", bucket: "photos", ok: true},
		{host: "photos.s3.example.com:9000", bucket: "photos", ok: true},
		{host: "PHOTOS.S3.Example.COM", bucket: "photos", ok: true},
		{host: "photos.s3.example.com.", bucket: "photos", ok: true},
		{host: "my.photos.s3.example.com", bucket: "my.photos", ok: true},
		{host: "b.s3.internal:80", bucket: "b", ok: true},
		// The longest domain wins: under example.com this would be "b.s3".
		{host: "b.s3.example.com", bucket: "b", ok: true},
		{host: "b.example.com", bucket: "b", ok: true},
		{host: "s3.example.com", ok: false},
		{host: "s3.example.com:9000", ok: false},
		{host: ".s3.example.com", ok: false},
		{host: "elsewhere.org", ok: false},
		{host: "xs3.example.com.evil", ok: false},
		{host: "127.0.0.1:9000", ok: false},
		{host: "[::1]:9000", ok: false},
		{host: "", ok: false},
	} {
		t.Run(tc.host, func(t *testing.T) {
			bucket, ok := handler.BucketFromHost(domains, tc.host)
			require.Equal(t, tc.ok, ok)
			require.Equal(t, tc.bucket, bucket)
		})
	}

	_, ok := handler.BucketFromHost(nil, "b.s3.example.com")
	require.False(t, ok, "no domains: path-style only")
}

// vhostRequest sends a request to h with Host set, as a virtual-host client
// would.
func vhostRequest(t *testing.T, h http.Handler, method, host, target, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Host = host
	req.Header.Set("Content-Length", strconv.Itoa(len(body)))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}

// TestVirtualHost_Rewrite: a request to a bucket host addresses that bucket
// with the path as its key, and path-style keeps working beside it.
func TestVirtualHost_Rewrite(t *testing.T) {
	h := handler.New(service.New(storagemem.New()), handler.WithDomains([]string{"s3.test"}))

	// PUT / on a bucket host creates the bucket.
	require.Equal(t, http.StatusOK, vhostRequest(t, h, http.MethodPut, "photos.s3.test", "/", "").Code)

	for _, key := range []string{"plain", "dir/a b+c", "dir/", "a//b"} {
		escaped := strings.ReplaceAll(strings.ReplaceAll(key, " ", "%20"), "+", "%2B")

		rec := vhostRequest(t, h, http.MethodPut, "photos.s3.test:9000", "/"+escaped, "body of "+key)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", key, rec.Body.String())

		// Path-style on the bare domain reads what the virtual host wrote.
		get := vhostRequest(t, h, http.MethodGet, "s3.test", "/photos/"+escaped, "")
		require.Equal(t, http.StatusOK, get.Code, key)
		require.Equal(t, "body of "+key, get.Body.String())
	}

	// An encoded slash stays part of the key rather than splitting it.
	require.Equal(t, http.StatusOK, vhostRequest(t, h, http.MethodPut, "photos.s3.test", "/x%2Fy", "slash").Code)
	require.Equal(t, "slash", vhostRequest(t, h, http.MethodGet, "s3.test", "/photos/x%2Fy", "").Body.String())

	// GET / on a bucket host lists the bucket; on the bare domain, buckets.
	var list struct {
		Name     string   `xml:"Name"`
		Contents []string `xml:"Contents>Key"`
	}

	rec := vhostRequest(t, h, http.MethodGet, "photos.s3.test", "/?list-type=2", "")
	require.Equal(t, http.StatusOK, rec.Code)
	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &list))
	require.Equal(t, "photos", list.Name)
	require.Len(t, list.Contents, 5)

	var buckets struct {
		Names []string `xml:"Buckets>Bucket>Name"`
	}

	rec = vhostRequest(t, h, http.MethodGet, "s3.test", "/", "")
	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &buckets))
	require.Equal(t, []string{"photos"}, buckets.Names)

	// A host under no domain is path-style.
	rec = vhostRequest(t, h, http.MethodGet, "photos.elsewhere", "/photos/plain", "")
	require.Equal(t, http.StatusOK, rec.Code)

	// A missing bucket named by host is NoSuchBucket naming it.
	rec = vhostRequest(t, h, http.MethodGet, "nowhere.s3.test", "/k", "")
	require.Equal(t, http.StatusNotFound, rec.Code)
	require.Contains(t, rec.Body.String(), "<BucketName>nowhere</BucketName>")
}
