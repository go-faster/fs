package main

import (
	"context"
	"strings"
	"time"

	"github.com/go-faster/errors"
	clientv3 "go.etcd.io/etcd/client/v3"
	"go.uber.org/zap"

	"github.com/go-faster/fs/auth"
	"github.com/go-faster/fs/internal/adminhandler"
	"github.com/go-faster/fs/internal/cluster/etcd"
	"github.com/go-faster/fs/internal/validate"
)

// etcdCredentialTimeout bounds a single admin credential write against etcd.
const etcdCredentialTimeout = 10 * time.Second

// clusterCredentials is the etcd-backed credential store: cluster-wide runtime
// key management. Credentials live under <prefix>/auth/keys/ with their secrets
// sealed by the cluster secret; every node runs one of these, watching that
// namespace and applying it to a live auth.Store — the same atomic-snapshot hot
// reload SIGHUP uses — so a key added, rotated or removed on any admin listener
// propagates to every node with no restart.
//
// It is authoritative on its own: config-file credentials are not merged. The
// bootstrap keys passed to newClusterCredentials seed the namespace only when it
// is empty, so an operator switching a node to etcd auth keeps their root
// credential, but etcd wins forever after.
//
// It satisfies adminhandler.CredentialManager.
type clusterCredentials struct {
	lg      *zap.Logger
	client  *clientv3.Client
	etcdCfg etcd.Config
	sealer  *auth.Sealer

	store  *auth.Store
	source *etcd.AuthSource
}

var (
	_ adminhandler.CredentialManager = (*clusterCredentials)(nil)
	_ adminhandler.PublicReadStore   = (*clusterCredentials)(nil)
)

// newClusterCredentials builds the store: it derives the sealer from the
// cluster secret, seeds bootstrap credentials and public-read buckets into an
// empty namespace, and starts the watch that keeps the live auth.Store in sync.
// The returned store serves a valid (possibly empty) snapshot immediately.
//
// bootstrapKeys and bootstrapPublicRead are the node config's credentials and
// public-read buckets; they seed a fresh cluster once, after which etcd is
// authoritative. Pass nil for both on a listener that only reads (the headless
// admin), so it never seeds.
func newClusterCredentials(
	ctx context.Context,
	lg *zap.Logger,
	client *clientv3.Client,
	etcdCfg etcd.Config,
	clusterSecret string,
	bootstrapPublicRead []string,
	bootstrapKeys []auth.Key,
) (*clusterCredentials, error) {
	sealer, err := auth.NewSealer([]byte(clusterSecret))
	if err != nil {
		return nil, errors.Wrap(err, "credential sealer")
	}

	// Start from an empty (but valid) store so the auth pipeline is wired before
	// the first snapshot arrives. Public-read buckets come from the watch, not
	// this initial value.
	store, err := auth.NewStore(auth.Config{})
	if err != nil {
		return nil, errors.Wrap(err, "credential store")
	}

	c := &clusterCredentials{
		lg:      lg,
		client:  client,
		etcdCfg: etcdCfg,
		sealer:  sealer,
		store:   store,
	}

	if err := c.seed(ctx, bootstrapKeys, bootstrapPublicRead); err != nil {
		return nil, err
	}

	// The watch is the single writer of the live store: NewAuthSource fires
	// onChange once with the current state before it returns, so the store is
	// populated by the time this constructor completes.
	source, err := etcd.NewAuthSource(ctx, client, etcdCfg, c.onChange)
	if err != nil {
		return nil, errors.Wrap(err, "watch cluster credentials")
	}

	source.OnError = func(err error) {
		c.lg.Warn("Cluster auth watch error; serving last known state", zap.Error(err))
	}

	c.source = source

	return c, nil
}

// Store returns the live auth store to wire into the S3 server.
func (c *clusterCredentials) Store() *auth.Store { return c.store }

// Close stops the watch.
func (c *clusterCredentials) Close() error {
	if c.source == nil {
		return nil
	}

	return c.source.Close()
}

// onChange rebuilds the live auth store from the full snapshot — credentials and
// public-read buckets alike. Records whose secret cannot be unsealed (wrong
// cluster secret, corruption) are skipped so one bad record never denies every
// credential; the rest of the snapshot stays live.
func (c *clusterCredentials) onChange(snap etcd.AuthSnapshot) {
	cfg := auth.Config{
		Keys:              make([]auth.Key, 0, len(snap.Records)),
		PublicReadBuckets: snap.PublicRead,
	}

	for _, rec := range snap.Records {
		secret, err := c.sealer.Open(rec.SecretSealed)
		if err != nil {
			c.lg.Warn("Skipping unreadable cluster credential",
				zap.String("access_key", rec.AccessKey), zap.Error(err))

			continue
		}

		grants, err := grantsFromRecord(rec.Grants)
		if err != nil {
			c.lg.Warn("Skipping cluster credential with invalid grant",
				zap.String("access_key", rec.AccessKey), zap.Error(err))

			continue
		}

		cfg.Keys = append(cfg.Keys, auth.Key{AccessKey: rec.AccessKey, SecretKey: secret, Grants: grants})
	}

	if err := c.store.Set(cfg); err != nil {
		// Keep the previous snapshot live rather than dropping all auth.
		c.lg.Error("Rejected cluster credential snapshot; keeping previous", zap.Error(err))
	}
}

// List returns the current credentials (secrets omitted) from the watch
// snapshot, sorted by access key. Every etcd credential is a managed one.
func (c *clusterCredentials) List() []auth.KeyInfo {
	records := c.source.Snapshot().Records

	infos := make([]auth.KeyInfo, 0, len(records))
	for _, rec := range records {
		grants, err := grantsFromRecord(rec.Grants)
		if err != nil {
			continue
		}

		infos = append(infos, auth.KeyInfo{
			AccessKey: rec.AccessKey,
			Grants:    grants,
			Source:    auth.SourceManaged,
			CreatedAt: rec.CreatedAt,
		})
	}

	return infos
}

// Create seals a new credential and stores it in etcd. The access key and/or
// secret are generated when omitted. The watch applies it to the live store on
// every node; the returned Created carries the secret (exposed only here).
func (c *clusterCredentials) Create(in auth.CreateInput) (*auth.Created, error) {
	access := in.AccessKey
	if access == "" {
		access = auth.NewAccessKey()
	}

	if err := validateAccessKey(access); err != nil {
		return nil, err
	}

	secret := in.SecretKey
	if secret == "" {
		secret = auth.NewSecretKey()
	}

	sealed, err := c.sealer.Seal(secret)
	if err != nil {
		return nil, errors.Wrap(err, "seal secret")
	}

	rec := etcd.AuthRecord{
		AccessKey:    access,
		SecretSealed: sealed,
		Grants:       grantsToRecord(in.Grants),
		CreatedAt:    time.Now().UTC(),
	}

	ctx, cancel := context.WithTimeout(context.Background(), etcdCredentialTimeout)
	defer cancel()

	created, err := etcd.CreateAuthKey(ctx, c.client, c.etcdCfg, rec)
	if err != nil {
		return nil, errors.Wrap(err, "store credential")
	}

	if !created {
		return nil, errors.Wrapf(auth.ErrKeyExists, "access key %q", access)
	}

	return &auth.Created{
		AccessKey: access,
		SecretKey: secret,
		Grants:    append([]auth.Grant(nil), in.Grants...),
		CreatedAt: rec.CreatedAt,
	}, nil
}

// PublicReadBuckets returns the cluster-wide public-read bucket list, read
// authoritatively from etcd so it reflects a just-applied change.
func (c *clusterCredentials) PublicReadBuckets(ctx context.Context) ([]string, error) {
	buckets, _, err := etcd.LoadPublicRead(ctx, c.client, c.etcdCfg)
	if err != nil {
		return nil, errors.Wrap(err, "read public-read buckets")
	}

	return buckets, nil
}

// SetPublicReadBuckets replaces the cluster-wide public-read bucket list. The
// watch applies it to the live store on every node.
func (c *clusterCredentials) SetPublicReadBuckets(ctx context.Context, buckets []string) error {
	for _, b := range buckets {
		if err := validate.BucketName(b); err != nil {
			return errors.Wrap(adminhandler.ErrPublicReadRejected, err.Error())
		}
	}

	if err := etcd.SetPublicRead(ctx, c.client, c.etcdCfg, buckets); err != nil {
		return errors.Wrap(err, "store public-read buckets")
	}

	return nil
}

// Delete removes a credential from etcd. The watch removes it from the live
// store on every node.
func (c *clusterCredentials) Delete(accessKey string) error {
	ctx, cancel := context.WithTimeout(context.Background(), etcdCredentialTimeout)
	defer cancel()

	deleted, err := etcd.DeleteAuthKey(ctx, c.client, c.etcdCfg, accessKey)
	if err != nil {
		return errors.Wrap(err, "delete credential")
	}

	if !deleted {
		return errors.Wrapf(auth.ErrKeyNotFound, "access key %q", accessKey)
	}

	return nil
}

// seed stores the bootstrap credentials and public-read buckets, but only into
// an empty namespace: on a cluster that already holds them, config is ignored
// (etcd is the one authoritative source). Every write is compare-and-set on
// absence, so two nodes racing to seed a fresh cluster converge on one set.
func (c *clusterCredentials) seed(ctx context.Context, bootstrapKeys []auth.Key, bootstrapPublicRead []string) error {
	if err := c.seedKeys(ctx, bootstrapKeys); err != nil {
		return err
	}

	return c.seedPublicRead(ctx, bootstrapPublicRead)
}

// seedKeys seeds the credential set when the keys namespace is empty.
func (c *clusterCredentials) seedKeys(ctx context.Context, bootstrap []auth.Key) error {
	if len(bootstrap) == 0 {
		return nil
	}

	existing, _, err := etcd.ListAuthKeys(ctx, c.client, c.etcdCfg)
	if err != nil {
		return errors.Wrap(err, "check existing credentials")
	}

	if len(existing) > 0 {
		return nil
	}

	seeded := 0

	for _, k := range bootstrap {
		sealed, err := c.sealer.Seal(k.SecretKey)
		if err != nil {
			return errors.Wrapf(err, "seal bootstrap key %q", k.AccessKey)
		}

		rec := etcd.AuthRecord{
			AccessKey:    k.AccessKey,
			SecretSealed: sealed,
			Grants:       grantsToRecord(k.Grants),
			CreatedAt:    time.Now().UTC(),
		}

		if _, err := etcd.CreateAuthKey(ctx, c.client, c.etcdCfg, rec); err != nil {
			return errors.Wrapf(err, "seed bootstrap key %q", k.AccessKey)
		}

		seeded++
	}

	c.lg.Info("Seeded cluster credentials from config", zap.Int("keys", seeded))

	return nil
}

// seedPublicRead seeds the public-read bucket list when none has been stored.
// An empty config list is skipped, so the etcd key stays absent (equivalent to
// empty) and a later config change can still seed it.
func (c *clusterCredentials) seedPublicRead(ctx context.Context, buckets []string) error {
	if len(buckets) == 0 {
		return nil
	}

	seeded, err := etcd.SeedPublicRead(ctx, c.client, c.etcdCfg, buckets)
	if err != nil {
		return errors.Wrap(err, "seed public-read buckets")
	}

	if seeded {
		c.lg.Info("Seeded cluster public-read buckets from config", zap.Int("buckets", len(buckets)))
	}

	return nil
}

// validateAccessKey rejects access keys that would break the flat etcd key
// namespace or produce ambiguous keys. Generated keys always pass.
func validateAccessKey(accessKey string) error {
	if accessKey == "" {
		return errors.New("access key must not be empty")
	}

	if strings.ContainsAny(accessKey, "/ \t\r\n") {
		return errors.Errorf("access key %q must not contain slashes or whitespace", accessKey)
	}

	return nil
}

// grantsToRecord converts auth grants to their stored (etcd) form.
func grantsToRecord(grants []auth.Grant) []etcd.AuthGrant {
	if len(grants) == 0 {
		return nil
	}

	out := make([]etcd.AuthGrant, 0, len(grants))
	for _, g := range grants {
		out = append(out, etcd.AuthGrant{Pattern: g.Pattern, Permission: g.Permission.String()})
	}

	return out
}

// grantsFromRecord converts stored grants back to auth grants, rejecting an
// unknown permission name.
func grantsFromRecord(grants []etcd.AuthGrant) ([]auth.Grant, error) {
	out := make([]auth.Grant, 0, len(grants))
	for _, g := range grants {
		perm, err := auth.ParsePermission(g.Permission)
		if err != nil {
			return nil, err
		}

		out = append(out, auth.Grant{Pattern: g.Pattern, Permission: perm})
	}

	return out, nil
}
