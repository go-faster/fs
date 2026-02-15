package handler_test

import (
	"context"
	"testing"

	"github.com/go-faster/errors"
	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

func TestHandler_DeleteObjects_Success(t *testing.T) {
	t.Parallel()

	const bucketName = "test-bucket"

	deletedKeys := []string{}

	svc := baseMock()
	svc.DeleteObjectFunc = func(ctx context.Context, bucket, key string) error {
		require.Equal(t, bucketName, bucket)

		deletedKeys = append(deletedKeys, key)

		return nil
	}

	ctx := t.Context()
	client := newTestClient(t, svc)

	// Delete multiple objects.
	objectsCh := make(chan minio.ObjectInfo)

	go func() {
		defer close(objectsCh)

		objectsCh <- minio.ObjectInfo{Key: "file1.txt"}

		objectsCh <- minio.ObjectInfo{Key: "file2.txt"}

		objectsCh <- minio.ObjectInfo{Key: "dir/file3.txt"}
	}()

	errorCh := client.RemoveObjects(ctx, bucketName, objectsCh, minio.RemoveObjectsOptions{})

	// Verify no errors.
	for err := range errorCh {
		require.NoError(t, err.Err, "Error deleting object: %s", err.ObjectName)
	}

	// Verify all objects were deleted.
	require.ElementsMatch(t, []string{"file1.txt", "file2.txt", "dir/file3.txt"}, deletedKeys)
}

func TestHandler_DeleteObjects_WithErrors(t *testing.T) {
	t.Parallel()

	const bucketName = "test-bucket"

	svc := baseMock()
	svc.DeleteObjectFunc = func(ctx context.Context, bucket, key string) error {
		if key == "error-file.txt" {
			return errors.New("simulated error")
		}

		return nil
	}

	ctx := t.Context()
	client := newTestClient(t, svc)

	// Delete multiple objects, one will fail.
	objectsCh := make(chan minio.ObjectInfo)

	go func() {
		defer close(objectsCh)

		objectsCh <- minio.ObjectInfo{Key: "success1.txt"}

		objectsCh <- minio.ObjectInfo{Key: "error-file.txt"}

		objectsCh <- minio.ObjectInfo{Key: "success2.txt"}
	}()

	errorCh := client.RemoveObjects(ctx, bucketName, objectsCh, minio.RemoveObjectsOptions{})

	// Count errors.
	errorCount := 0

	for err := range errorCh {
		if err.Err != nil {
			errorCount++

			require.Equal(t, "error-file.txt", err.ObjectName)
		}
	}

	require.Equal(t, 1, errorCount, "Expected exactly one error")
}

func TestHandler_DeleteObjects_AllFail(t *testing.T) {
	t.Parallel()

	const bucketName = "test-bucket"

	expectedError := errors.New("permission denied")

	svc := baseMock()
	svc.DeleteObjectFunc = func(ctx context.Context, bucket, key string) error {
		return expectedError
	}

	ctx := t.Context()
	client := newTestClient(t, svc)

	// Delete multiple objects, all will fail.
	objectsCh := make(chan minio.ObjectInfo)

	go func() {
		defer close(objectsCh)

		objectsCh <- minio.ObjectInfo{Key: "file1.txt"}

		objectsCh <- minio.ObjectInfo{Key: "file2.txt"}

		objectsCh <- minio.ObjectInfo{Key: "file3.txt"}
	}()

	errorCh := client.RemoveObjects(ctx, bucketName, objectsCh, minio.RemoveObjectsOptions{})

	// Count errors.
	errorCount := 0

	for err := range errorCh {
		if err.Err != nil {
			errorCount++

			require.Contains(t, err.Err.Error(), "permission denied")
		}
	}

	require.Equal(t, 3, errorCount, "Expected all 3 deletions to fail")
}

func TestHandler_DeleteObjects_Empty(t *testing.T) {
	t.Parallel()

	svc := baseMock()
	deleteCallCount := 0
	svc.DeleteObjectFunc = func(ctx context.Context, bucket, key string) error {
		deleteCallCount++
		return nil
	}

	ctx := t.Context()
	client := newTestClient(t, svc)

	// Delete empty list.
	objectsCh := make(chan minio.ObjectInfo)
	close(objectsCh) // Close immediately - empty list

	errorCh := client.RemoveObjects(ctx, "test-bucket", objectsCh, minio.RemoveObjectsOptions{})

	// Should have no errors.
	for err := range errorCh {
		require.NoError(t, err.Err)
	}

	// DeleteObject should not be called for empty list.
	require.Equal(t, 0, deleteCallCount, "DeleteObject should not be called for empty list")
}

func TestHandler_DeleteObjects_SingleObject(t *testing.T) {
	t.Parallel()

	const (
		bucketName = "test-bucket"
		objectKey  = "single-file.txt"
	)

	deleted := false

	svc := baseMock()
	svc.DeleteObjectFunc = func(ctx context.Context, bucket, key string) error {
		require.Equal(t, bucketName, bucket)
		require.Equal(t, objectKey, key)

		deleted = true

		return nil
	}

	ctx := t.Context()
	client := newTestClient(t, svc)

	// Delete single object.
	objectsCh := make(chan minio.ObjectInfo)

	go func() {
		defer close(objectsCh)

		objectsCh <- minio.ObjectInfo{Key: objectKey}
	}()

	errorCh := client.RemoveObjects(ctx, bucketName, objectsCh, minio.RemoveObjectsOptions{})

	// Verify no errors.
	for err := range errorCh {
		require.NoError(t, err.Err)
	}

	require.True(t, deleted, "Object should have been deleted")
}

func TestHandler_DeleteObjects_NestedKeys(t *testing.T) {
	t.Parallel()

	const bucketName = "test-bucket"

	deletedKeys := []string{}

	svc := baseMock()
	svc.DeleteObjectFunc = func(ctx context.Context, bucket, key string) error {
		deletedKeys = append(deletedKeys, key)
		return nil
	}

	ctx := t.Context()
	client := newTestClient(t, svc)

	// Delete objects with nested paths.
	objectsCh := make(chan minio.ObjectInfo)

	go func() {
		defer close(objectsCh)

		objectsCh <- minio.ObjectInfo{Key: "level1/file1.txt"}

		objectsCh <- minio.ObjectInfo{Key: "level1/level2/file2.txt"}

		objectsCh <- minio.ObjectInfo{Key: "level1/level2/level3/file3.txt"}
	}()

	errorCh := client.RemoveObjects(ctx, bucketName, objectsCh, minio.RemoveObjectsOptions{})

	// Verify no errors.
	for err := range errorCh {
		require.NoError(t, err.Err)
	}

	require.ElementsMatch(t, []string{
		"level1/file1.txt",
		"level1/level2/file2.txt",
		"level1/level2/level3/file3.txt",
	}, deletedKeys)
}

func TestHandler_DeleteObjects_SpecialCharacters(t *testing.T) {
	t.Parallel()

	const bucketName = "test-bucket"

	deletedKeys := []string{}

	svc := baseMock()
	svc.DeleteObjectFunc = func(ctx context.Context, bucket, key string) error {
		deletedKeys = append(deletedKeys, key)
		return nil
	}

	ctx := t.Context()
	client := newTestClient(t, svc)

	// Delete objects with special characters in keys.
	objectsCh := make(chan minio.ObjectInfo)

	go func() {
		defer close(objectsCh)

		objectsCh <- minio.ObjectInfo{Key: "file-with-dashes.txt"}

		objectsCh <- minio.ObjectInfo{Key: "file_with_underscores.txt"}

		objectsCh <- minio.ObjectInfo{Key: "file.with.dots.txt"}

		objectsCh <- minio.ObjectInfo{Key: "file with spaces.txt"}
	}()

	errorCh := client.RemoveObjects(ctx, bucketName, objectsCh, minio.RemoveObjectsOptions{})

	// Verify no errors.
	for err := range errorCh {
		require.NoError(t, err.Err)
	}

	require.ElementsMatch(t, []string{
		"file-with-dashes.txt",
		"file_with_underscores.txt",
		"file.with.dots.txt",
		"file with spaces.txt",
	}, deletedKeys)
}

func TestHandler_DeleteObjects_ManyObjects(t *testing.T) {
	t.Parallel()

	const (
		bucketName = "test-bucket"
		numObjects = 100
	)

	deletedKeys := make(map[string]bool)

	svc := baseMock()
	svc.DeleteObjectFunc = func(ctx context.Context, bucket, key string) error {
		deletedKeys[key] = true
		return nil
	}

	ctx := t.Context()
	client := newTestClient(t, svc)

	// Delete many objects.
	objectsCh := make(chan minio.ObjectInfo)

	go func() {
		defer close(objectsCh)

		for i := 0; i < numObjects; i++ {
			objectsCh <- minio.ObjectInfo{Key: "file" + string(rune('0'+i%10)) + ".txt"}
		}
	}()

	errorCh := client.RemoveObjects(ctx, bucketName, objectsCh, minio.RemoveObjectsOptions{})

	// Verify no errors.
	errorCount := 0

	for err := range errorCh {
		if err.Err != nil {
			errorCount++
		}
	}

	require.Equal(t, 0, errorCount, "No errors expected")
	require.Len(t, deletedKeys, 10, "Should have deleted 10 unique files")
}

func TestHandler_DeleteObjects_BucketNotFound(t *testing.T) {
	t.Parallel()

	svc := baseMock()
	svc.DeleteObjectFunc = func(ctx context.Context, bucket, key string) error {
		return fs.ErrBucketNotFound
	}

	ctx := t.Context()
	client := newTestClient(t, svc)

	// Try to delete objects from non-existent bucket.
	objectsCh := make(chan minio.ObjectInfo)

	go func() {
		defer close(objectsCh)

		objectsCh <- minio.ObjectInfo{Key: "file.txt"}
	}()

	errorCh := client.RemoveObjects(ctx, "nonexistent-bucket", objectsCh, minio.RemoveObjectsOptions{})

	// Should get error.
	errorCount := 0

	for err := range errorCh {
		if err.Err != nil {
			errorCount++

			require.Contains(t, err.Err.Error(), "bucket")
		}
	}

	require.Equal(t, 1, errorCount, "Expected bucket not found error")
}

func TestHandler_DeleteObjects_MixedSuccess(t *testing.T) {
	t.Parallel()

	const bucketName = "test-bucket"

	deletedKeys := []string{}
	failedKeys := []string{"fail1.txt", "fail3.txt"}

	svc := baseMock()
	svc.DeleteObjectFunc = func(ctx context.Context, bucket, key string) error {
		for _, failKey := range failedKeys {
			if key == failKey {
				return errors.New("simulated failure for " + key)
			}
		}

		deletedKeys = append(deletedKeys, key)

		return nil
	}

	ctx := t.Context()
	client := newTestClient(t, svc)

	// Delete objects with some failures.
	objectsCh := make(chan minio.ObjectInfo)

	go func() {
		defer close(objectsCh)

		objectsCh <- minio.ObjectInfo{Key: "success1.txt"}

		objectsCh <- minio.ObjectInfo{Key: "fail1.txt"}

		objectsCh <- minio.ObjectInfo{Key: "success2.txt"}

		objectsCh <- minio.ObjectInfo{Key: "fail3.txt"}

		objectsCh <- minio.ObjectInfo{Key: "success3.txt"}
	}()

	errorCh := client.RemoveObjects(ctx, bucketName, objectsCh, minio.RemoveObjectsOptions{})

	// Count successes and failures.
	errorCount := 0

	for err := range errorCh {
		if err.Err != nil {
			errorCount++
		}
	}

	require.Equal(t, 2, errorCount, "Expected 2 errors")
	require.ElementsMatch(t, []string{"success1.txt", "success2.txt", "success3.txt"}, deletedKeys)
}

func TestHandler_DeleteObjects_DuplicateKeys(t *testing.T) {
	t.Parallel()

	const bucketName = "test-bucket"

	deleteCount := make(map[string]int)

	svc := baseMock()
	svc.DeleteObjectFunc = func(ctx context.Context, bucket, key string) error {
		deleteCount[key]++
		return nil
	}

	ctx := t.Context()
	client := newTestClient(t, svc)

	// Delete same key multiple times.
	objectsCh := make(chan minio.ObjectInfo)

	go func() {
		defer close(objectsCh)

		objectsCh <- minio.ObjectInfo{Key: "duplicate.txt"}

		objectsCh <- minio.ObjectInfo{Key: "duplicate.txt"}

		objectsCh <- minio.ObjectInfo{Key: "duplicate.txt"}
	}()

	errorCh := client.RemoveObjects(ctx, bucketName, objectsCh, minio.RemoveObjectsOptions{})

	// Verify no errors.
	for err := range errorCh {
		require.NoError(t, err.Err)
	}

	// Verify the key was deleted 3 times (handler doesn't deduplicate).
	require.Equal(t, 3, deleteCount["duplicate.txt"])
}

func TestHandler_DeleteObjects_ObjectNotFound(t *testing.T) {
	t.Parallel()

	const bucketName = "test-bucket"

	svc := baseMock()
	svc.DeleteObjectFunc = func(ctx context.Context, bucket, key string) error {
		if key == "nonexistent.txt" {
			return fs.ErrObjectNotFound
		}

		return nil
	}

	ctx := t.Context()
	client := newTestClient(t, svc)

	// Delete objects including non-existent one.
	objectsCh := make(chan minio.ObjectInfo)

	go func() {
		defer close(objectsCh)

		objectsCh <- minio.ObjectInfo{Key: "exists.txt"}

		objectsCh <- minio.ObjectInfo{Key: "nonexistent.txt"}
	}()

	errorCh := client.RemoveObjects(ctx, bucketName, objectsCh, minio.RemoveObjectsOptions{})

	// Should have one error for nonexistent object.
	errorCount := 0

	for err := range errorCh {
		if err.Err != nil {
			errorCount++

			require.Equal(t, "nonexistent.txt", err.ObjectName)
		}
	}

	require.Equal(t, 1, errorCount, "Expected one error for non-existent object")
}

func TestHandler_DeleteObjects_EmptyKeys(t *testing.T) {
	t.Parallel()

	const bucketName = "test-bucket"

	deletedKeys := []string{}

	svc := baseMock()
	svc.DeleteObjectFunc = func(ctx context.Context, bucket, key string) error {
		deletedKeys = append(deletedKeys, key)
		return nil
	}

	ctx := t.Context()
	client := newTestClient(t, svc)

	// Delete objects including empty key.
	objectsCh := make(chan minio.ObjectInfo)

	go func() {
		defer close(objectsCh)

		objectsCh <- minio.ObjectInfo{Key: ""}

		objectsCh <- minio.ObjectInfo{Key: "valid.txt"}
	}()

	errorCh := client.RemoveObjects(ctx, bucketName, objectsCh, minio.RemoveObjectsOptions{})

	// Verify no errors (empty key is passed through).
	for err := range errorCh {
		require.NoError(t, err.Err)
	}

	require.ElementsMatch(t, []string{"", "valid.txt"}, deletedKeys)
}
