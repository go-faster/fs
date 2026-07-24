package adminhandler

import (
	"context"
	"net/http"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/internal/adminapi"
)

// ReloadResult reports what a reload applied and the config revision left in
// effect.
type ReloadResult struct {
	// Reloaded names the hot-reloadable parts the reload applied, any of
	// "credentials", "tls".
	Reloaded []string
	// ConfigRevision is the config file's revision marker after the reload;
	// empty when the config sets none.
	ConfigRevision string
}

// Reloader re-applies hot-reloadable configuration on demand — the same work
// SIGHUP does. Implemented by the S3 data node; absent (nil) on a listener with
// nothing to reload (the headless cluster admin serves no S3 data), where the
// endpoint returns 501.
type Reloader interface {
	// Reload re-reads the config file and applies the hot-reloadable parts,
	// returning what it changed and the config revision now in effect.
	Reload(ctx context.Context) (ReloadResult, error)
}

// ReloadConfig re-applies the hot-reloadable configuration and reports what
// changed.
func (a *AdminAPI) ReloadConfig(ctx context.Context) (*adminapi.ReloadResult, error) {
	if a.opts.Reloader == nil {
		return nil, apiErr(http.StatusNotImplemented,
			errors.New("configuration reload is not available on this admin listener"))
	}

	res, err := a.opts.Reloader.Reload(ctx)
	if err != nil {
		return nil, apiErr(http.StatusInternalServerError, err)
	}

	out := &adminapi.ReloadResult{Reloaded: res.Reloaded}
	if res.ConfigRevision != "" {
		out.ConfigRevision = adminapi.NewOptString(res.ConfigRevision)
	}

	return out, nil
}
