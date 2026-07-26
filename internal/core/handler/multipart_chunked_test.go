package handler_test

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
	"github.com/go-faster/fs/internal/core/handler"
	"github.com/go-faster/fs/internal/mock"
)

// TestUploadPart_ChunkedEncodingDeclaresTheDecodedSize pins the size a part
// upload declares to the backend.
//
// A streaming-signature upload frames its payload in chunks, so Content-Length
// counts the framing while X-Amz-Decoded-Content-Length counts the object
// bytes. The reader the handler builds strips the framing, so declaring
// Content-Length tells a backend to expect bytes that will never arrive.
//
// The assertion is on what the backend is told rather than on what comes back,
// because single-node backends copy until EOF and ignore the declared size:
// they store the part correctly whichever length is passed, which is precisely
// how this survived until a cluster — which must know an object's size before
// it can place a single fragment — failed every multipart upload from such a
// client.
func TestUploadPart_ChunkedEncodingDeclaresTheDecodedSize(t *testing.T) {
	const bucket, key = "bucket-a", "big.bin"

	// Six bytes of payload inside sixteen bytes of framing.
	const (
		body    = "6\r\nchunky\r\n0\r\n\r\n"
		decoded = 6
	)

	var got fs.UploadPartRequest

	svc := &mock.StorageMock{
		CreateMultipartUploadFunc: func(_ context.Context, req *fs.CreateMultipartUploadRequest) (*fs.MultipartUpload, error) {
			return &fs.MultipartUpload{Bucket: req.Bucket, Key: req.Key, UploadID: "upload-1"}, nil
		},
		UploadPartFunc: func(_ context.Context, req *fs.UploadPartRequest) (*fs.Part, error) {
			got = *req

			// Drain it the way a backend that trusts the size would, so the
			// test also shows there is nothing more to read.
			read, _ := io.Copy(io.Discard, req.Reader)

			return &fs.Part{PartNumber: req.PartNumber, ETag: "etag-1", Size: read}, nil
		},
	}

	h := handler.New(svc)

	create := do(t, h, http.MethodPost, "/"+bucket+"/"+key+"?uploads", "", nil)
	require.Equal(t, http.StatusOK, create.Code)

	var initiated handler.InitiateMultipartUploadResult
	require.NoError(t, xml.Unmarshal(create.Body.Bytes(), &initiated))

	part := do(t, h, http.MethodPut,
		"/"+bucket+"/"+key+"?partNumber=1&uploadId="+initiated.UploadID, body, map[string]string{
			"Content-Encoding":             "aws-chunked",
			"X-Amz-Content-Sha256":         "STREAMING-UNSIGNED-PAYLOAD-TRAILER",
			"X-Amz-Decoded-Content-Length": "6",
		})
	require.Equal(t, http.StatusOK, part.Code)

	assert.Equal(t, int64(decoded), got.Size,
		"the part must be sized by its decoded length, not by the framing around it")
	assert.Equal(t, 1, got.PartNumber)
}
