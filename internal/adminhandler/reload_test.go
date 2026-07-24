package adminhandler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/adminapi"
)

// fakeReloader is a Reloader whose result and error are fixed by the test.
type fakeReloader struct {
	result ReloadResult
	err    error
	calls  int
}

func (f *fakeReloader) Reload(context.Context) (ReloadResult, error) {
	f.calls++

	return f.result, f.err
}

func TestReloadConfig(t *testing.T) {
	rel := &fakeReloader{result: ReloadResult{Reloaded: []string{"credentials", "tls"}, ConfigRevision: "cfg-abc"}}
	api := NewAdminAPI(Options{Reloader: rel})

	res, err := api.ReloadConfig(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, rel.calls)
	assert.Equal(t, []string{"credentials", "tls"}, res.Reloaded)
	assert.Equal(t, "cfg-abc", res.ConfigRevision.Or(""))
}

// TestReloadConfigNoRevision covers a config that sets no revision marker: the
// field is omitted rather than sent empty.
func TestReloadConfigNoRevision(t *testing.T) {
	api := NewAdminAPI(Options{Reloader: &fakeReloader{result: ReloadResult{Reloaded: []string{"credentials"}}}})

	res, err := api.ReloadConfig(context.Background())
	require.NoError(t, err)
	assert.False(t, res.ConfigRevision.Set)
}

// TestReloadConfigUnavailable pins the 501 on a listener with nothing to
// reload — the headless cluster admin, which has no Reloader.
func TestReloadConfigUnavailable(t *testing.T) {
	api := NewAdminAPI(Options{})

	_, err := api.ReloadConfig(context.Background())
	require.Error(t, err)

	var status *adminapi.ErrorStatusCode
	require.ErrorAs(t, err, &status)
	assert.Equal(t, 501, status.StatusCode)
}

func TestReloadConfigError(t *testing.T) {
	api := NewAdminAPI(Options{Reloader: &fakeReloader{err: errors.New("bad config")}})

	_, err := api.ReloadConfig(context.Background())
	require.Error(t, err)

	var status *adminapi.ErrorStatusCode
	require.ErrorAs(t, err, &status)
	assert.Equal(t, 500, status.StatusCode)
}

// TestGetInfoConfigRevision covers GetInfo reporting the loaded revision, and
// omitting it when no source is set.
func TestGetInfoConfigRevision(t *testing.T) {
	withRev := NewAdminAPI(Options{
		StartTime:      time.Now(),
		ConfigRevision: func() string { return "cfg-99" },
	})

	info, err := withRev.GetInfo(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "cfg-99", info.ConfigRevision.Or(""))

	none, err := NewAdminAPI(Options{StartTime: time.Now()}).GetInfo(context.Background())
	require.NoError(t, err)
	assert.False(t, none.ConfigRevision.Set, "no source reports no revision")
}
