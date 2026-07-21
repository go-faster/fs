package handler_test

import (
	"encoding/xml"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/core/handler"
)

// minPartBody is a part at exactly the S3 5 MiB minimum size.
func minPartBody() string {
	return strings.Repeat("a", 5*1024*1024)
}

// initiateUpload starts a multipart upload over the wire and returns its ID.
//
//nolint:unparam // bucket kept for call-site readability.
func initiateUpload(t *testing.T, h http.Handler, bucket, key string) string {
	t.Helper()

	rec := do(t, h, http.MethodPost, "/"+bucket+"/"+key+"?uploads", "", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var result handler.InitiateMultipartUploadResult
	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &result))
	require.NotEmpty(t, result.UploadID)

	return result.UploadID
}

// putPart uploads one part and returns its quoted ETag.
//
//nolint:unparam // bucket kept for call-site readability.
func putPart(t *testing.T, h http.Handler, bucket, key, uploadID string, partNumber int, body string) string {
	t.Helper()

	target := fmt.Sprintf("/%s/%s?partNumber=%d&uploadId=%s", bucket, key, partNumber, uploadID)
	rec := do(t, h, http.MethodPut, target, body, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	etag := rec.Header().Get("ETag")
	require.NotEmpty(t, etag)

	return etag
}

// completeBody renders a CompleteMultipartUpload request body from
// (partNumber, quotedETag) pairs.
func completeBody(parts ...[2]string) string {
	var b strings.Builder

	b.WriteString("<CompleteMultipartUpload>")

	for _, p := range parts {
		b.WriteString("<Part><PartNumber>" + p[0] + "</PartNumber><ETag>" + p[1] + "</ETag></Part>")
	}

	b.WriteString("</CompleteMultipartUpload>")

	return b.String()
}

func errorCode(t *testing.T, body string) string {
	t.Helper()

	var e struct {
		Code string `xml:"Code"`
	}
	require.NoError(t, xml.Unmarshal([]byte(body), &e))

	return e.Code
}

func TestMultipart_CompleteValidation(t *testing.T) {
	const bucket, key = "bucket-a", "big.bin"

	newUpload := func(t *testing.T) (http.Handler, string) {
		h := newStorageHandler(t)
		require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket, "", nil).Code)

		return h, initiateUpload(t, h, bucket, key)
	}

	complete := func(t *testing.T, h http.Handler, uploadID, body string) *httptest.ResponseRecorder {
		return do(t, h, http.MethodPost, "/"+bucket+"/"+key+"?uploadId="+uploadID, body, nil)
	}

	t.Run("Success", func(t *testing.T) {
		h, uploadID := newUpload(t)
		etag1 := putPart(t, h, bucket, key, uploadID, 1, minPartBody())
		etag2 := putPart(t, h, bucket, key, uploadID, 2, "tail")

		rec := complete(t, h, uploadID, completeBody([2]string{"1", etag1}, [2]string{"2", etag2}))
		require.Equal(t, http.StatusOK, rec.Code)

		var result handler.CompleteMultipartUploadResult
		require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &result))
		require.True(t, strings.HasSuffix(result.ETag, `-2"`), "multipart ETag %q must end in part count", result.ETag)

		// The assembled object must be readable with the concatenated content.
		get := do(t, h, http.MethodGet, "/"+bucket+"/"+key, "", nil)
		require.Equal(t, http.StatusOK, get.Code)
		require.Equal(t, int64(5*1024*1024+4), int64(get.Body.Len()))
	})

	t.Run("EntityTooSmall", func(t *testing.T) {
		h, uploadID := newUpload(t)
		// A non-last part below 5 MiB must be rejected.
		etag1 := putPart(t, h, bucket, key, uploadID, 1, "too small")
		etag2 := putPart(t, h, bucket, key, uploadID, 2, "tail")

		rec := complete(t, h, uploadID, completeBody([2]string{"1", etag1}, [2]string{"2", etag2}))
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Equal(t, "EntityTooSmall", errorCode(t, rec.Body.String()))
	})

	t.Run("SmallLastPartAllowed", func(t *testing.T) {
		h, uploadID := newUpload(t)
		etag1 := putPart(t, h, bucket, key, uploadID, 1, "only part")

		rec := complete(t, h, uploadID, completeBody([2]string{"1", etag1}))
		require.Equal(t, http.StatusOK, rec.Code)
	})

	t.Run("InvalidPart_WrongETag", func(t *testing.T) {
		h, uploadID := newUpload(t)
		putPart(t, h, bucket, key, uploadID, 1, "data")

		rec := complete(t, h, uploadID, completeBody([2]string{"1", `"deadbeef"`}))
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Equal(t, "InvalidPart", errorCode(t, rec.Body.String()))
	})

	t.Run("InvalidPart_NeverUploaded", func(t *testing.T) {
		h, uploadID := newUpload(t)
		// Part 1 is valid and large enough; part 2 was never uploaded and must
		// win over any size complaint.
		etag1 := putPart(t, h, bucket, key, uploadID, 1, minPartBody())

		rec := complete(t, h, uploadID, completeBody([2]string{"1", etag1}, [2]string{"2", etag1}))
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Equal(t, "InvalidPart", errorCode(t, rec.Body.String()))
	})

	t.Run("InvalidPartOrder", func(t *testing.T) {
		h, uploadID := newUpload(t)
		etag1 := putPart(t, h, bucket, key, uploadID, 1, minPartBody())
		etag2 := putPart(t, h, bucket, key, uploadID, 2, "tail")

		rec := complete(t, h, uploadID, completeBody([2]string{"2", etag2}, [2]string{"1", etag1}))
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Equal(t, "InvalidPartOrder", errorCode(t, rec.Body.String()))
	})

	t.Run("EmptyPartList", func(t *testing.T) {
		h, uploadID := newUpload(t)

		rec := complete(t, h, uploadID, "<CompleteMultipartUpload></CompleteMultipartUpload>")
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Equal(t, "MalformedXML", errorCode(t, rec.Body.String()))
	})
}

func TestMultipart_UploadPartNumberValidation(t *testing.T) {
	const bucket, key = "bucket-a", "big.bin"

	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket, "", nil).Code)
	uploadID := initiateUpload(t, h, bucket, key)

	for _, part := range []string{"0", "-1", "10001", "abc"} {
		t.Run(part, func(t *testing.T) {
			target := "/" + bucket + "/" + key + "?partNumber=" + part + "&uploadId=" + uploadID
			rec := do(t, h, http.MethodPut, target, "data", nil)
			require.Equal(t, http.StatusBadRequest, rec.Code)
			require.Equal(t, "InvalidArgument", errorCode(t, rec.Body.String()))
		})
	}

	// Boundary part numbers are accepted.
	putPart(t, h, bucket, key, uploadID, 1, "data")
	putPart(t, h, bucket, key, uploadID, 10000, "data")
}

func TestMultipart_ListParts(t *testing.T) {
	const bucket, key = "bucket-a", "big.bin"

	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket, "", nil).Code)
	uploadID := initiateUpload(t, h, bucket, key)

	etag2 := putPart(t, h, bucket, key, uploadID, 2, "bb")
	etag1 := putPart(t, h, bucket, key, uploadID, 1, "a")
	etag3 := putPart(t, h, bucket, key, uploadID, 3, "ccc")

	listParts := func(t *testing.T, query string) handler.ListPartsResult {
		t.Helper()

		rec := do(t, h, http.MethodGet, "/"+bucket+"/"+key+"?uploadId="+uploadID+query, "", nil)
		require.Equal(t, http.StatusOK, rec.Code)

		var result handler.ListPartsResult
		require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &result))

		return result
	}

	t.Run("All", func(t *testing.T) {
		result := listParts(t, "")
		require.Equal(t, bucket, result.Bucket)
		require.Equal(t, key, result.Key)
		require.Equal(t, uploadID, result.UploadID)
		require.False(t, result.IsTruncated)
		require.Len(t, result.Parts, 3)

		// Sorted ascending regardless of upload order, sizes and ETags intact.
		for i, expected := range []struct {
			etag string
			size int64
		}{{etag1, 1}, {etag2, 2}, {etag3, 3}} {
			require.Equal(t, i+1, result.Parts[i].PartNumber)
			require.Equal(t, expected.etag, result.Parts[i].ETag)
			require.Equal(t, expected.size, result.Parts[i].Size)
			require.False(t, result.Parts[i].LastModified.IsZero())
		}
	})

	t.Run("Paginated", func(t *testing.T) {
		page := listParts(t, "&max-parts=2")
		require.True(t, page.IsTruncated)
		require.Len(t, page.Parts, 2)
		require.Equal(t, 2, page.NextPartNumberMarker)

		rest := listParts(t, "&max-parts=2&part-number-marker=2")
		require.False(t, rest.IsTruncated)
		require.Len(t, rest.Parts, 1)
		require.Equal(t, 3, rest.Parts[0].PartNumber)
	})

	t.Run("InvalidMaxParts", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/"+bucket+"/"+key+"?uploadId="+uploadID+"&max-parts=abc", "", nil)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Equal(t, "InvalidArgument", errorCode(t, rec.Body.String()))
	})

	t.Run("UnknownUpload", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/"+bucket+"/"+key+"?uploadId=nonexistent", "", nil)
		require.Equal(t, http.StatusNotFound, rec.Code)
		require.Equal(t, "NoSuchUpload", errorCode(t, rec.Body.String()))
	})
}

func TestMultipart_ListMultipartUploads(t *testing.T) {
	const bucket = "bucket-a"

	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket, "", nil).Code)

	idDocs := initiateUpload(t, h, bucket, "docs/readme.txt")
	idA := initiateUpload(t, h, bucket, "a.bin")
	idB := initiateUpload(t, h, bucket, "b.bin")

	list := func(t *testing.T, query string) handler.ListMultipartUploadsResult {
		t.Helper()

		rec := do(t, h, http.MethodGet, "/"+bucket+"?uploads"+query, "", nil)
		require.Equal(t, http.StatusOK, rec.Code)

		var result handler.ListMultipartUploadsResult
		require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &result))

		return result
	}

	t.Run("All", func(t *testing.T) {
		result := list(t, "")
		require.Equal(t, bucket, result.Bucket)
		require.False(t, result.IsTruncated)
		require.Len(t, result.Uploads, 3)

		// Ordered by key.
		require.Equal(t, "a.bin", result.Uploads[0].Key)
		require.Equal(t, idA, result.Uploads[0].UploadID)
		require.Equal(t, "b.bin", result.Uploads[1].Key)
		require.Equal(t, idB, result.Uploads[1].UploadID)
		require.Equal(t, "docs/readme.txt", result.Uploads[2].Key)
		require.Equal(t, idDocs, result.Uploads[2].UploadID)
	})

	t.Run("Prefix", func(t *testing.T) {
		result := list(t, "&prefix=docs/")
		require.Len(t, result.Uploads, 1)
		require.Equal(t, "docs/readme.txt", result.Uploads[0].Key)
	})

	t.Run("Delimiter", func(t *testing.T) {
		result := list(t, "&delimiter=/")
		require.Len(t, result.Uploads, 2)
		require.Len(t, result.CommonPrefixes, 1)
		require.Equal(t, "docs/", result.CommonPrefixes[0].Prefix)
	})

	t.Run("Paginated", func(t *testing.T) {
		page := list(t, "&max-uploads=1")
		require.True(t, page.IsTruncated)
		require.Len(t, page.Uploads, 1)
		require.Equal(t, "a.bin", page.NextKeyMarker)
		require.Equal(t, idA, page.NextUploadIDMarker)

		rest := list(t, "&key-marker="+page.NextKeyMarker+"&upload-id-marker="+page.NextUploadIDMarker)
		require.False(t, rest.IsTruncated)
		require.Len(t, rest.Uploads, 2)
		require.Equal(t, "b.bin", rest.Uploads[0].Key)
	})

	t.Run("BucketNotFound", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/nonexistent?uploads", "", nil)
		require.Equal(t, http.StatusNotFound, rec.Code)
		require.Equal(t, "NoSuchBucket", errorCode(t, rec.Body.String()))
	})

	t.Run("GoneAfterAbort", func(t *testing.T) {
		id := initiateUpload(t, h, bucket, "temp.bin")
		rec := do(t, h, http.MethodDelete, "/"+bucket+"/temp.bin?uploadId="+id, "", nil)
		require.Equal(t, http.StatusNoContent, rec.Code)

		result := list(t, "&prefix=temp")
		require.Empty(t, result.Uploads)
	})
}

func TestMultipart_UploadPartCopy(t *testing.T) {
	const bucket, key = "bucket-a", "assembled.bin"

	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket, "", nil).Code)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket+"/src.bin", "0123456789", nil).Code)

	uploadID := initiateUpload(t, h, bucket, key)

	copyPart := func(t *testing.T, partNumber int, headers map[string]string) *httptest.ResponseRecorder {
		t.Helper()

		target := fmt.Sprintf("/%s/%s?partNumber=%d&uploadId=%s", bucket, key, partNumber, uploadID)

		return do(t, h, http.MethodPut, target, "", headers)
	}

	t.Run("FullSource", func(t *testing.T) {
		rec := copyPart(t, 1, map[string]string{"X-Amz-Copy-Source": "/" + bucket + "/src.bin"})
		require.Equal(t, http.StatusOK, rec.Code)

		var result struct {
			ETag string `xml:"ETag"`
		}
		require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &result))
		require.NotEmpty(t, result.ETag)

		// The copied part shows up in ListParts with the source's full size.
		list := do(t, h, http.MethodGet, "/"+bucket+"/"+key+"?uploadId="+uploadID, "", nil)

		var parts handler.ListPartsResult
		require.NoError(t, xml.Unmarshal(list.Body.Bytes(), &parts))
		require.Len(t, parts.Parts, 1)
		require.Equal(t, int64(10), parts.Parts[0].Size)
		require.Equal(t, result.ETag, parts.Parts[0].ETag)
	})

	t.Run("Range", func(t *testing.T) {
		rec := copyPart(t, 2, map[string]string{
			"X-Amz-Copy-Source":       "/" + bucket + "/src.bin",
			"X-Amz-Copy-Source-Range": "bytes=2-5",
		})
		require.Equal(t, http.StatusOK, rec.Code)

		list := do(t, h, http.MethodGet, "/"+bucket+"/"+key+"?uploadId="+uploadID, "", nil)

		var parts handler.ListPartsResult
		require.NoError(t, xml.Unmarshal(list.Body.Bytes(), &parts))
		require.Len(t, parts.Parts, 2)
		require.Equal(t, int64(4), parts.Parts[1].Size)
	})

	t.Run("MalformedRange", func(t *testing.T) {
		for _, rangeSpec := range []string{"2-5", "bytes=5-2", "bytes=2-", "bytes=-5", "bytes=a-b"} {
			rec := copyPart(t, 3, map[string]string{
				"X-Amz-Copy-Source":       "/" + bucket + "/src.bin",
				"X-Amz-Copy-Source-Range": rangeSpec,
			})
			require.Equal(t, http.StatusBadRequest, rec.Code, "range %q", rangeSpec)
			require.Equal(t, "InvalidArgument", errorCode(t, rec.Body.String()))
		}
	})

	t.Run("RangeBeyondSource", func(t *testing.T) {
		rec := copyPart(t, 3, map[string]string{
			"X-Amz-Copy-Source":       "/" + bucket + "/src.bin",
			"X-Amz-Copy-Source-Range": "bytes=5-20",
		})
		require.Equal(t, http.StatusRequestedRangeNotSatisfiable, rec.Code)
		require.Equal(t, "InvalidRange", errorCode(t, rec.Body.String()))
	})

	t.Run("MissingSource", func(t *testing.T) {
		rec := copyPart(t, 3, map[string]string{"X-Amz-Copy-Source": "/" + bucket + "/nonexistent"})
		require.Equal(t, http.StatusNotFound, rec.Code)
		require.Equal(t, "NoSuchKey", errorCode(t, rec.Body.String()))
	})
}
