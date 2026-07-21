package handler_test

import (
	"encoding/xml"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/core/handler"
)

// metaHeader reads an x-amz-meta-* response header. The handler emits these
// with all-lowercase names (matching AWS), bypassing Go's canonicalization,
// so recorder-based tests must read the raw header map.
func metaHeader(h http.Header, key string) string {
	if v := h["x-amz-meta-"+key]; len(v) > 0 {
		return v[0]
	}

	return ""
}

func TestMetadata_HeaderRoundTrip(t *testing.T) {
	const bucket = "bucket-a"

	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket, "", nil).Code)

	put := do(t, h, http.MethodPut, "/"+bucket+"/doc.txt", "content", map[string]string{
		"Content-Type":        "text/plain; charset=utf-8",
		"Cache-Control":       "max-age=3600",
		"Content-Disposition": `attachment; filename="doc.txt"`,
		"X-Amz-Meta-Color":    "blue",
		"X-Amz-Meta-Owner":    "tests",
	})
	require.Equal(t, http.StatusOK, put.Code)
	// PUT now reports the stored ETag.
	require.NotEmpty(t, put.Header().Get("ETag"))

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rec := do(t, h, method, "/"+bucket+"/doc.txt", "", nil)
		require.Equal(t, http.StatusOK, rec.Code)
		require.Equal(t, "text/plain; charset=utf-8", rec.Header().Get("Content-Type"), method)
		require.Equal(t, "max-age=3600", rec.Header().Get("Cache-Control"), method)
		require.Equal(t, `attachment; filename="doc.txt"`, rec.Header().Get("Content-Disposition"), method)
		require.Equal(t, "blue", metaHeader(rec.Header(), "color"), method)
		require.Equal(t, "tests", metaHeader(rec.Header(), "owner"), method)
	}
}

func TestMetadata_DefaultContentType(t *testing.T) {
	const bucket = "bucket-a"

	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket, "", nil).Code)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket+"/plain", "x", nil).Code)

	rec := do(t, h, http.MethodGet, "/"+bucket+"/plain", "", nil)
	require.Equal(t, "application/octet-stream", rec.Header().Get("Content-Type"))
}

func TestMetadata_AWSChunkedEncodingNotStored(t *testing.T) {
	const bucket = "bucket-a"

	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket, "", nil).Code)

	// aws-chunked is a transport detail and must not survive as metadata;
	// real encodings listed alongside it must.
	body := "3\r\nfoo\r\n0\r\n\r\n"
	put := do(t, h, http.MethodPut, "/"+bucket+"/enc.bin", body, map[string]string{
		"Content-Encoding":             "aws-chunked,gzip",
		"X-Amz-Content-Sha256":         "STREAMING-UNSIGNED-PAYLOAD-TRAILER",
		"X-Amz-Decoded-Content-Length": "3",
	})
	require.Equal(t, http.StatusOK, put.Code)

	rec := do(t, h, http.MethodGet, "/"+bucket+"/enc.bin", "", nil)
	require.Equal(t, "gzip", rec.Header().Get("Content-Encoding"))
	require.Equal(t, "foo", rec.Body.String())
}

func TestMetadata_MultipartRoundTrip(t *testing.T) {
	const bucket, key = "bucket-a", "big.bin"

	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket, "", nil).Code)

	rec := do(t, h, http.MethodPost, "/"+bucket+"/"+key+"?uploads", "", map[string]string{
		"Content-Type":     "video/mp4",
		"X-Amz-Meta-Title": "clip",
	})
	require.Equal(t, http.StatusOK, rec.Code)

	var result handler.InitiateMultipartUploadResult
	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &result))

	etag := putPart(t, h, bucket, key, result.UploadID, 1, "data")
	complete := do(t, h, http.MethodPost, "/"+bucket+"/"+key+"?uploadId="+result.UploadID,
		completeBody([2]string{"1", etag}), nil)
	require.Equal(t, http.StatusOK, complete.Code)

	get := do(t, h, http.MethodGet, "/"+bucket+"/"+key, "", nil)
	require.Equal(t, "video/mp4", get.Header().Get("Content-Type"))
	require.Equal(t, "clip", metaHeader(get.Header(), "title"))
}

func TestCopyObject_MetadataDirectives(t *testing.T) {
	const bucket = "bucket-a"

	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket, "", nil).Code)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket+"/src.txt", "content", map[string]string{
		"Content-Type":     "text/plain",
		"X-Amz-Meta-Color": "blue",
	}).Code)

	t.Run("DefaultCopiesMetadata", func(t *testing.T) {
		rec := do(t, h, http.MethodPut, "/"+bucket+"/dst.txt", "", map[string]string{
			"X-Amz-Copy-Source": "/" + bucket + "/src.txt",
		})
		require.Equal(t, http.StatusOK, rec.Code)

		get := do(t, h, http.MethodGet, "/"+bucket+"/dst.txt", "", nil)
		require.Equal(t, "text/plain", get.Header().Get("Content-Type"))
		require.Equal(t, "blue", metaHeader(get.Header(), "color"))
	})

	t.Run("ReplaceUsesRequestHeaders", func(t *testing.T) {
		rec := do(t, h, http.MethodPut, "/"+bucket+"/dst2.txt", "", map[string]string{
			"X-Amz-Copy-Source":        "/" + bucket + "/src.txt",
			"X-Amz-Metadata-Directive": "REPLACE",
			"Content-Type":             "application/json",
			"X-Amz-Meta-Shape":         "round",
		})
		require.Equal(t, http.StatusOK, rec.Code)

		get := do(t, h, http.MethodGet, "/"+bucket+"/dst2.txt", "", nil)
		require.Equal(t, "application/json", get.Header().Get("Content-Type"))
		require.Equal(t, "round", metaHeader(get.Header(), "shape"))
		require.Empty(t, metaHeader(get.Header(), "color"))
	})

	t.Run("CopyToSelfWithoutReplace", func(t *testing.T) {
		rec := do(t, h, http.MethodPut, "/"+bucket+"/src.txt", "", map[string]string{
			"X-Amz-Copy-Source": "/" + bucket + "/src.txt",
		})
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Equal(t, "InvalidRequest", errorCode(t, rec.Body.String()))
	})

	t.Run("CopyToSelfWithReplace", func(t *testing.T) {
		rec := do(t, h, http.MethodPut, "/"+bucket+"/src.txt", "", map[string]string{
			"X-Amz-Copy-Source":        "/" + bucket + "/src.txt",
			"X-Amz-Metadata-Directive": "REPLACE",
			"Content-Type":             "text/markdown",
		})
		require.Equal(t, http.StatusOK, rec.Code)

		get := do(t, h, http.MethodGet, "/"+bucket+"/src.txt", "", nil)
		require.Equal(t, "text/markdown", get.Header().Get("Content-Type"))
		require.Equal(t, "content", get.Body.String())
	})

	t.Run("InvalidDirective", func(t *testing.T) {
		rec := do(t, h, http.MethodPut, "/"+bucket+"/dst3.txt", "", map[string]string{
			"X-Amz-Copy-Source":        "/" + bucket + "/src.txt",
			"X-Amz-Metadata-Directive": "MERGE",
		})
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Equal(t, "InvalidArgument", errorCode(t, rec.Body.String()))
	})
}

// taggingBody renders a Tagging XML document.
func taggingBody(pairs ...[2]string) string {
	var b strings.Builder

	b.WriteString("<Tagging><TagSet>")

	for _, p := range pairs {
		b.WriteString("<Tag><Key>" + p[0] + "</Key><Value>" + p[1] + "</Value></Tag>")
	}

	b.WriteString("</TagSet></Tagging>")

	return b.String()
}

func TestObjectTagging_Endpoints(t *testing.T) {
	const bucket = "bucket-a"

	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket, "", nil).Code)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket+"/obj.txt", "content", nil).Code)

	getTags := func(t *testing.T) handler.Tagging {
		t.Helper()

		rec := do(t, h, http.MethodGet, "/"+bucket+"/obj.txt?tagging", "", nil)
		require.Equal(t, http.StatusOK, rec.Code)

		var doc handler.Tagging
		require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &doc))

		return doc
	}

	t.Run("EmptyByDefault", func(t *testing.T) {
		require.Empty(t, getTags(t).TagSet.Tags)
	})

	t.Run("PutGetDelete", func(t *testing.T) {
		rec := do(t, h, http.MethodPut, "/"+bucket+"/obj.txt?tagging",
			taggingBody([2]string{"env", "prod"}, [2]string{"team", "storage"}), nil)
		require.Equal(t, http.StatusOK, rec.Code)

		doc := getTags(t)
		require.Equal(t, []handler.TagXML{{Key: "env", Value: "prod"}, {Key: "team", Value: "storage"}}, doc.TagSet.Tags)

		require.Equal(t, http.StatusNoContent, do(t, h, http.MethodDelete, "/"+bucket+"/obj.txt?tagging", "", nil).Code)
		require.Empty(t, getTags(t).TagSet.Tags)
	})

	t.Run("PutWithTaggingHeader", func(t *testing.T) {
		rec := do(t, h, http.MethodPut, "/"+bucket+"/tagged.txt", "x", map[string]string{
			"X-Amz-Tagging": "k1=v1&k2=v%202",
		})
		require.Equal(t, http.StatusOK, rec.Code)

		get := do(t, h, http.MethodGet, "/"+bucket+"/tagged.txt?tagging", "", nil)

		var doc handler.Tagging
		require.NoError(t, xml.Unmarshal(get.Body.Bytes(), &doc))
		require.Equal(t, []handler.TagXML{{Key: "k1", Value: "v1"}, {Key: "k2", Value: "v 2"}}, doc.TagSet.Tags)
	})

	t.Run("ObjectNotFound", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/"+bucket+"/missing.txt?tagging", "", nil)
		require.Equal(t, http.StatusNotFound, rec.Code)
		require.Equal(t, "NoSuchKey", errorCode(t, rec.Body.String()))
	})

	t.Run("InvalidTag", func(t *testing.T) {
		// 11 tags exceed the object limit of 10.
		var pairs [][2]string
		for i := range 11 {
			pairs = append(pairs, [2]string{"k" + string(rune('a'+i)), "v"})
		}

		for name, body := range map[string]string{
			"TooMany":      taggingBody(pairs...),
			"LongKey":      taggingBody([2]string{strings.Repeat("k", 129), "v"}),
			"LongValue":    taggingBody([2]string{"k", strings.Repeat("v", 257)}),
			"DuplicateKey": taggingBody([2]string{"k", "v1"}, [2]string{"k", "v2"}),
		} {
			rec := do(t, h, http.MethodPut, "/"+bucket+"/obj.txt?tagging", body, nil)
			require.Equal(t, http.StatusBadRequest, rec.Code, name)
			require.Equal(t, "InvalidTag", errorCode(t, rec.Body.String()), name)
		}
	})
}
