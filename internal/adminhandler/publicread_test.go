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

// fakePublicReadStore is an in-memory PublicReadStore.
type fakePublicReadStore struct {
	buckets []string
	reject  bool
}

func (s *fakePublicReadStore) PublicReadBuckets(_ context.Context) ([]string, error) {
	return s.buckets, nil
}

func (s *fakePublicReadStore) SetPublicReadBuckets(_ context.Context, buckets []string) error {
	if s.reject {
		return errors.Wrap(ErrPublicReadRejected, "bad bucket name")
	}

	s.buckets = buckets

	return nil
}

func TestPublicReadEndpointsDisabledWithoutStore(t *testing.T) {
	a := NewAdminAPI(Options{}) // no PublicRead (file auth / single node)

	_, err := a.GetPublicReadBuckets(t.Context())
	requireStatusCode(t, err, http.StatusNotImplemented)

	_, err = a.SetPublicReadBuckets(t.Context(), &adminapi.SetPublicReadBucketsRequest{})
	requireStatusCode(t, err, http.StatusNotImplemented)
}

func TestGetPublicReadBuckets(t *testing.T) {
	store := &fakePublicReadStore{buckets: []string{"assets", "downloads"}}
	a := NewAdminAPI(Options{PublicRead: store})

	got, err := a.GetPublicReadBuckets(t.Context())
	require.NoError(t, err)
	assert.Equal(t, []string{"assets", "downloads"}, got.Buckets)
}

func TestGetPublicReadBucketsEmptyIsNonNil(t *testing.T) {
	a := NewAdminAPI(Options{PublicRead: &fakePublicReadStore{}})

	got, err := a.GetPublicReadBuckets(t.Context())
	require.NoError(t, err)
	assert.NotNil(t, got.Buckets)
	assert.Empty(t, got.Buckets)
}

func TestSetPublicReadBuckets(t *testing.T) {
	store := &fakePublicReadStore{buckets: []string{"old"}}
	a := NewAdminAPI(Options{PublicRead: store})

	got, err := a.SetPublicReadBuckets(t.Context(), &adminapi.SetPublicReadBucketsRequest{
		Buckets: []string{"assets"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"assets"}, got.Buckets)
	assert.Equal(t, []string{"assets"}, store.buckets)
}

func TestSetPublicReadBucketsRejected(t *testing.T) {
	a := NewAdminAPI(Options{PublicRead: &fakePublicReadStore{reject: true}})

	_, err := a.SetPublicReadBuckets(t.Context(), &adminapi.SetPublicReadBucketsRequest{
		Buckets: []string{"Invalid_Name"},
	})
	requireStatusCode(t, err, http.StatusBadRequest)
}
