package handler_test

import (
	"encoding/xml"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/core/handler"
)

// listBucket issues a bucket GET with the given query and decodes the result.
//
//nolint:unparam // bucket kept for call-site readability.
func listBucket(t *testing.T, h http.Handler, bucket, query string) handler.ListBucketResult {
	t.Helper()

	rec := do(t, h, http.MethodGet, "/"+bucket+query, "", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var result handler.ListBucketResult
	require.NoError(t, xml.Unmarshal(rec.Body.Bytes(), &result))

	return result
}

func TestListObjects_EncodingType(t *testing.T) {
	const bucket = "bucket-a"

	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket, "", nil).Code)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket+"/dir%20x/a%2Bb%20c.txt", "x", nil).Code)

	t.Run("KeysEncodedAfterSorting", func(t *testing.T) {
		// "+" and space are percent-encoded; "/" is preserved.
		result := listBucket(t, h, bucket, "?list-type=2&encoding-type=url")
		require.Equal(t, "url", result.EncodingType)
		require.Len(t, result.Contents, 1)
		require.Equal(t, "dir%20x/a%2Bb%20c.txt", result.Contents[0].Key)
	})

	t.Run("PrefixAndDelimiterEncoded", func(t *testing.T) {
		result := listBucket(t, h, bucket, "?list-type=2&encoding-type=url&delimiter=%2F&prefix=dir%20")
		require.Equal(t, "dir%20", result.Prefix)
		// "/" is in the encoder's safe set, so a "/" delimiter is unchanged.
		require.Equal(t, "/", result.Delimiter)
		require.Len(t, result.CommonPrefixes, 1)
		require.Equal(t, "dir%20x/", result.CommonPrefixes[0].Prefix)
	})

	t.Run("NoEncodingByDefault", func(t *testing.T) {
		result := listBucket(t, h, bucket, "?list-type=2")
		require.Empty(t, result.EncodingType)
		require.Len(t, result.Contents, 1)
		require.Equal(t, "dir x/a+b c.txt", result.Contents[0].Key)
	})

	t.Run("InvalidEncodingType", func(t *testing.T) {
		rec := do(t, h, http.MethodGet, "/"+bucket+"?encoding-type=base64", "", nil)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Equal(t, "InvalidArgument", errorCode(t, rec.Body.String()))
	})
}

func TestListObjects_MaxKeys(t *testing.T) {
	const bucket = "bucket-a"

	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket, "", nil).Code)

	for _, key := range []string{"a", "b", "c"} {
		require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket+"/"+key, "x", nil).Code)
	}

	t.Run("ClampedTo1000", func(t *testing.T) {
		result := listBucket(t, h, bucket, "?max-keys=5000")
		require.Equal(t, 1000, result.MaxKeys)
		require.Len(t, result.Contents, 3)
	})

	t.Run("Zero", func(t *testing.T) {
		result := listBucket(t, h, bucket, "?max-keys=0")
		require.Equal(t, 0, result.MaxKeys)
		require.Empty(t, result.Contents)
		require.False(t, result.IsTruncated)
	})

	t.Run("Invalid", func(t *testing.T) {
		for _, v := range []string{"abc", "-1"} {
			rec := do(t, h, http.MethodGet, "/"+bucket+"?max-keys="+v, "", nil)
			require.Equal(t, http.StatusBadRequest, rec.Code, "max-keys=%s", v)
			require.Equal(t, "InvalidArgument", errorCode(t, rec.Body.String()))
		}
	})
}

func TestListObjectsV1_Markers(t *testing.T) {
	const bucket = "bucket-a"

	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket, "", nil).Code)

	for _, key := range []string{"a", "b", "c"} {
		require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket+"/"+key, "x", nil).Code)
	}

	t.Run("MarkerIsExclusive", func(t *testing.T) {
		result := listBucket(t, h, bucket, "?marker=a")
		require.Len(t, result.Contents, 2)
		require.Equal(t, "b", result.Contents[0].Key)
		require.Equal(t, "a", result.Marker)
	})

	t.Run("NoNextMarkerWithoutDelimiter", func(t *testing.T) {
		result := listBucket(t, h, bucket, "?max-keys=2")
		require.True(t, result.IsTruncated)
		require.Empty(t, result.NextMarker)
		// V1 must not carry the V2-only KeyCount.
		require.Nil(t, result.KeyCount)
	})

	t.Run("NextMarkerWithDelimiter", func(t *testing.T) {
		result := listBucket(t, h, bucket, "?max-keys=2&delimiter=/")
		require.True(t, result.IsTruncated)
		require.Equal(t, "b", result.NextMarker)
	})
}

func TestListObjectsV2_Semantics(t *testing.T) {
	const bucket = "bucket-a"

	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket, "", nil).Code)

	for _, key := range []string{"dir/x", "dir/y", "a", "b"} {
		require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket+"/"+key, "x", nil).Code)
	}

	t.Run("KeyCountIncludesCommonPrefixes", func(t *testing.T) {
		result := listBucket(t, h, bucket, "?list-type=2&delimiter=/")
		require.NotNil(t, result.KeyCount)
		require.Equal(t, 3, *result.KeyCount) // a, b + dir/
		require.Len(t, result.Contents, 2)
		require.Len(t, result.CommonPrefixes, 1)
	})

	t.Run("KeyCountZeroOnEmpty", func(t *testing.T) {
		result := listBucket(t, h, bucket, "?list-type=2&prefix=nothing")
		require.NotNil(t, result.KeyCount)
		require.Equal(t, 0, *result.KeyCount)
	})

	t.Run("StartAfter", func(t *testing.T) {
		result := listBucket(t, h, bucket, "?list-type=2&start-after=b")
		require.Len(t, result.Contents, 2)
		require.Equal(t, "dir/x", result.Contents[0].Key)
		require.Equal(t, "b", result.StartAfter)
	})

	t.Run("ContinuationTokenFlow", func(t *testing.T) {
		page := listBucket(t, h, bucket, "?list-type=2&max-keys=2")
		require.True(t, page.IsTruncated)
		require.NotEmpty(t, page.NextContinuationToken)
		require.Len(t, page.Contents, 2)

		rest := listBucket(t, h, bucket, "?list-type=2&continuation-token="+page.NextContinuationToken)
		require.False(t, rest.IsTruncated)
		require.Len(t, rest.Contents, 2)
		require.Equal(t, "dir/x", rest.Contents[0].Key)
	})
}

// TestListObjects_DelimiterOrdering guards delimiter-aware ordering: the common
// prefix "1/" sorts before the key "1999", so pagination boundaries fall in the
// right place.
func TestListObjects_DelimiterOrdering(t *testing.T) {
	const bucket = "bucket-a"

	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket, "", nil).Code)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket+"/1/x", "x", nil).Code)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket+"/1999", "x", nil).Code)

	page := listBucket(t, h, bucket, "?delimiter=/&max-keys=1")
	require.True(t, page.IsTruncated)
	require.Empty(t, page.Contents)
	require.Len(t, page.CommonPrefixes, 1)
	require.Equal(t, "1/", page.CommonPrefixes[0].Prefix)
	require.Equal(t, "1/", page.NextMarker)

	rest := listBucket(t, h, bucket, "?delimiter=/&marker="+page.NextMarker)
	require.False(t, rest.IsTruncated)
	require.Len(t, rest.Contents, 1)
	require.Equal(t, "1999", rest.Contents[0].Key)
	require.Empty(t, rest.CommonPrefixes)
}

// TestListObjects_CommonPrefixesCountTowardMaxKeys: prefixes and keys share the
// max-keys budget.
func TestListObjects_CommonPrefixesCountTowardMaxKeys(t *testing.T) {
	const bucket = "bucket-a"

	h := newStorageHandler(t)
	require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket, "", nil).Code)

	for _, key := range []string{"a/1", "b/2", "c"} {
		require.Equal(t, http.StatusOK, do(t, h, http.MethodPut, "/"+bucket+"/"+key, "x", nil).Code)
	}

	page := listBucket(t, h, bucket, "?list-type=2&delimiter=/&max-keys=2")
	require.True(t, page.IsTruncated)
	require.Len(t, page.CommonPrefixes, 2)
	require.Empty(t, page.Contents)
	require.NotNil(t, page.KeyCount)
	require.Equal(t, 2, *page.KeyCount)
}
