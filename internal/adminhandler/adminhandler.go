// Package adminhandler implements the go-faster/fs admin API: instance info and
// runtime access-key management, backed by an auth.Manager.
package adminhandler

import (
	"context"
	"net/http"
	"runtime"
	"time"

	"github.com/go-faster/errors"

	"github.com/go-faster/fs/auth"
	"github.com/go-faster/fs/internal/adminapi"
)

// BuildInfo is static build metadata reported by GetInfo.
type BuildInfo struct {
	Version string
	Commit  string
}

// Options configures an AdminAPI.
type Options struct {
	// Manager is the access-key store to manage. Required.
	Manager *auth.Manager
	// Build is reported by GetInfo.
	Build BuildInfo
	// AuthEnabled reports whether the S3 server enforces SigV4.
	AuthEnabled bool
	// StartTime is the process start, for uptime. Defaults to now.
	StartTime time.Time
	// now overrides the clock in tests.
	now func() time.Time
}

// AdminAPI implements adminapi.Handler.
type AdminAPI struct {
	opts Options
}

var _ adminapi.Handler = (*AdminAPI)(nil)

// NewAdminAPI builds an AdminAPI. It panics if no Manager is provided.
func NewAdminAPI(opts Options) *AdminAPI {
	if opts.Manager == nil {
		panic("adminhandler: Manager is required")
	}

	if opts.now == nil {
		opts.now = time.Now
	}

	if opts.StartTime.IsZero() {
		opts.StartTime = opts.now()
	}

	return &AdminAPI{opts: opts}
}

// GetInfo returns build info and uptime.
func (a *AdminAPI) GetInfo(_ context.Context) (*adminapi.InstanceInfo, error) {
	return &adminapi.InstanceInfo{
		Version:       a.opts.Build.Version,
		Commit:        a.opts.Build.Commit,
		GoVersion:     runtime.Version(),
		Os:            runtime.GOOS,
		Arch:          runtime.GOARCH,
		StartTime:     a.opts.StartTime,
		UptimeSeconds: a.opts.now().Sub(a.opts.StartTime).Seconds(),
		AuthEnabled:   a.opts.AuthEnabled,
	}, nil
}

// ListAccessKeys returns all credentials, secrets omitted.
func (a *AdminAPI) ListAccessKeys(_ context.Context) (*adminapi.AccessKeyList, error) {
	infos := a.opts.Manager.List()

	keys := make([]adminapi.AccessKey, 0, len(infos))
	for _, info := range infos {
		key := adminapi.AccessKey{
			AccessKey: info.AccessKey,
			Grants:    grantsToAPI(info.Grants),
			Source:    sourceToAPI(info.Source),
		}

		if !info.CreatedAt.IsZero() {
			key.CreatedAt = adminapi.NewOptDateTime(info.CreatedAt)
		}

		keys = append(keys, key)
	}

	return &adminapi.AccessKeyList{Keys: keys}, nil
}

// CreateAccessKey creates a runtime credential.
func (a *AdminAPI) CreateAccessKey(_ context.Context, req *adminapi.CreateAccessKeyRequest) (*adminapi.CreatedAccessKey, error) {
	grants, err := grantsFromAPI(req.Grants)
	if err != nil {
		return nil, apiErr(http.StatusBadRequest, err)
	}

	created, err := a.opts.Manager.Create(auth.CreateInput{
		AccessKey: req.AccessKey.Or(""),
		SecretKey: req.SecretKey.Or(""),
		Grants:    grants,
	})
	if err != nil {
		if errors.Is(err, auth.ErrKeyExists) {
			return nil, apiErr(http.StatusConflict, err)
		}

		return nil, apiErr(http.StatusBadRequest, err)
	}

	return &adminapi.CreatedAccessKey{
		AccessKey: created.AccessKey,
		SecretKey: created.SecretKey,
		Grants:    grantsToAPI(created.Grants),
		CreatedAt: created.CreatedAt,
	}, nil
}

// DeleteAccessKey removes a runtime credential.
func (a *AdminAPI) DeleteAccessKey(_ context.Context, params adminapi.DeleteAccessKeyParams) error {
	err := a.opts.Manager.Delete(params.AccessKey)
	switch {
	case err == nil:
		return nil
	case errors.Is(err, auth.ErrKeyNotFound):
		return apiErr(http.StatusNotFound, err)
	case errors.Is(err, auth.ErrKeyImmutable):
		return apiErr(http.StatusForbidden, err)
	default:
		return apiErr(http.StatusInternalServerError, err)
	}
}

// NewError maps an unhandled error to a 500 structured response.
func (a *AdminAPI) NewError(_ context.Context, err error) *adminapi.ErrorStatusCode {
	return apiErr(http.StatusInternalServerError, err)
}

func grantsToAPI(grants []auth.Grant) []adminapi.Grant {
	out := make([]adminapi.Grant, 0, len(grants))
	for _, g := range grants {
		out = append(out, adminapi.Grant{Bucket: g.Pattern, Permission: permissionToAPI(g.Permission)})
	}

	return out
}

func grantsFromAPI(grants []adminapi.Grant) ([]auth.Grant, error) {
	out := make([]auth.Grant, 0, len(grants))
	for _, g := range grants {
		perm, err := permissionFromAPI(g.Permission)
		if err != nil {
			return nil, err
		}

		bucket := g.Bucket
		if bucket == "" {
			return nil, errors.New("grant bucket pattern must not be empty")
		}

		out = append(out, auth.Grant{Pattern: bucket, Permission: perm})
	}

	return out, nil
}

func permissionToAPI(p auth.Permission) adminapi.Permission {
	switch p {
	case auth.Write:
		return adminapi.PermissionWrite
	case auth.Admin:
		return adminapi.PermissionAdmin
	case auth.Read:
		return adminapi.PermissionRead
	default:
		return adminapi.PermissionRead
	}
}

func permissionFromAPI(p adminapi.Permission) (auth.Permission, error) {
	switch p {
	case adminapi.PermissionRead:
		return auth.Read, nil
	case adminapi.PermissionWrite:
		return auth.Write, nil
	case adminapi.PermissionAdmin:
		return auth.Admin, nil
	default:
		return auth.Read, errors.Errorf("unknown permission %q", p)
	}
}

func sourceToAPI(s auth.Source) adminapi.Source {
	if s == auth.SourceManaged {
		return adminapi.SourceManaged
	}

	return adminapi.SourceConfig
}
