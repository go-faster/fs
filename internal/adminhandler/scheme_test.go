package adminhandler

import (
	"context"
	"net/http"
	"testing"

	"github.com/go-faster/errors"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/adminapi"
)

// fakeSchemeStore is an in-memory BucketSchemeStore keyed by bucket name.
type fakeSchemeStore struct {
	overrides map[string]string
	exists    map[string]bool
	reject    bool
}

func (s *fakeSchemeStore) SchemeOverride(_ context.Context, bucket string) (string, error) {
	if !s.exists[bucket] {
		return "", errors.Wrap(ErrBucketNotFound, bucket)
	}

	return s.overrides[bucket], nil
}

func (s *fakeSchemeStore) SetScheme(_ context.Context, bucket, scheme string) error {
	if !s.exists[bucket] {
		return errors.Wrap(ErrBucketNotFound, bucket)
	}

	if s.reject {
		return errors.Wrap(ErrSchemeRejected, "topology cannot host scheme "+scheme)
	}

	s.overrides[bucket] = scheme

	return nil
}

func TestBucketSchemeEndpointsDisabledWithoutStore(t *testing.T) {
	a := NewAdminAPI(Options{}) // no BucketSchemes (single-node / headless without cluster)

	_, err := a.GetBucketScheme(t.Context(), adminapi.GetBucketSchemeParams{Bucket: "b"})
	requireStatusCode(t, err, http.StatusNotImplemented)

	_, err = a.SetBucketScheme(t.Context(), &adminapi.SetBucketSchemeRequest{}, adminapi.SetBucketSchemeParams{Bucket: "b"})
	requireStatusCode(t, err, http.StatusNotImplemented)
}

func TestGetBucketSchemeDefault(t *testing.T) {
	store := &fakeSchemeStore{overrides: map[string]string{}, exists: map[string]bool{"media": true}}
	a := NewAdminAPI(Options{BucketSchemes: store, ClusterDefaultScheme: "rf2.5"})

	got, err := a.GetBucketScheme(t.Context(), adminapi.GetBucketSchemeParams{Bucket: "media"})
	require.NoError(t, err)

	assert.Equal(t, "media", got.Bucket)
	assert.Equal(t, "rf2.5", got.Scheme)
	assert.Equal(t, "rf2.5", got.ClusterDefault)
	assert.True(t, got.IsDefault)
	assert.False(t, got.Override.Set) // no explicit override
}

func TestGetBucketSchemeOverride(t *testing.T) {
	store := &fakeSchemeStore{
		overrides: map[string]string{"media": "ec:4,2"},
		exists:    map[string]bool{"media": true},
	}
	a := NewAdminAPI(Options{BucketSchemes: store, ClusterDefaultScheme: "rf2.5"})

	got, err := a.GetBucketScheme(t.Context(), adminapi.GetBucketSchemeParams{Bucket: "media"})
	require.NoError(t, err)

	assert.Equal(t, "ec:4,2", got.Scheme)
	assert.Equal(t, "ec:4,2", got.Override.Or(""))
	assert.Equal(t, "rf2.5", got.ClusterDefault)
	assert.False(t, got.IsDefault)
}

func TestGetBucketSchemeNotFound(t *testing.T) {
	store := &fakeSchemeStore{overrides: map[string]string{}, exists: map[string]bool{}}
	a := NewAdminAPI(Options{BucketSchemes: store, ClusterDefaultScheme: "rf2.5"})

	_, err := a.GetBucketScheme(t.Context(), adminapi.GetBucketSchemeParams{Bucket: "missing"})
	requireStatusCode(t, err, http.StatusNotFound)
}

func TestSetBucketSchemeOverrideThenClear(t *testing.T) {
	store := &fakeSchemeStore{overrides: map[string]string{}, exists: map[string]bool{"media": true}}
	a := NewAdminAPI(Options{BucketSchemes: store, ClusterDefaultScheme: "rf2.5"})

	// Set an override.
	got, err := a.SetBucketScheme(t.Context(),
		&adminapi.SetBucketSchemeRequest{Scheme: adminapi.NewOptString("ec:4,2")},
		adminapi.SetBucketSchemeParams{Bucket: "media"})
	require.NoError(t, err)
	assert.Equal(t, "ec:4,2", got.Scheme)
	assert.False(t, got.IsDefault)

	// "default" clears it back to the cluster default.
	got, err = a.SetBucketScheme(t.Context(),
		&adminapi.SetBucketSchemeRequest{Scheme: adminapi.NewOptString("default")},
		adminapi.SetBucketSchemeParams{Bucket: "media"})
	require.NoError(t, err)
	assert.Equal(t, "rf2.5", got.Scheme)
	assert.True(t, got.IsDefault)
	assert.Empty(t, store.overrides["media"])
}

func TestSetBucketSchemeRejected(t *testing.T) {
	store := &fakeSchemeStore{overrides: map[string]string{}, exists: map[string]bool{"media": true}, reject: true}
	a := NewAdminAPI(Options{BucketSchemes: store, ClusterDefaultScheme: "rf2.5"})

	_, err := a.SetBucketScheme(t.Context(),
		&adminapi.SetBucketSchemeRequest{Scheme: adminapi.NewOptString("ec:99,1")},
		adminapi.SetBucketSchemeParams{Bucket: "media"})
	requireStatusCode(t, err, http.StatusBadRequest)
}

func TestSetBucketSchemeNotFound(t *testing.T) {
	store := &fakeSchemeStore{overrides: map[string]string{}, exists: map[string]bool{}}
	a := NewAdminAPI(Options{BucketSchemes: store, ClusterDefaultScheme: "rf2.5"})

	_, err := a.SetBucketScheme(t.Context(),
		&adminapi.SetBucketSchemeRequest{Scheme: adminapi.NewOptString("rf3")},
		adminapi.SetBucketSchemeParams{Bucket: "missing"})
	requireStatusCode(t, err, http.StatusNotFound)
}
