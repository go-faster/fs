package etcd

import (
	"context"
	"strconv"

	"github.com/go-faster/errors"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// SchemaVersion is the cluster schema version this binary implements: the
// on-disk record formats (object sidecars, bucket records) and the etcd
// control-plane layout, taken together as one monotonic version. Bump it and
// add a migration (see Migration) whenever a change to either format is not
// backward-compatible for a node running the previous version.
const SchemaVersion = 1

// ErrSchemaTooNew reports that the cluster's schema version is newer than this
// binary implements: the binary is too old to safely join, so it refuses to
// start rather than misread or corrupt a format it does not understand. This
// is the guard that keeps a stale binary (e.g. a botched rollback) out of an
// already-migrated cluster.
var ErrSchemaTooNew = errors.New("cluster schema is newer than this binary")

// schemaVersionKey holds the cluster's agreed schema version.
func (c Config) schemaVersionKey() string { return c.Prefix + "/meta/schema-version" }

// EnsureCompatible reconciles this binary's schema version with the cluster's
// and reports the cluster's version. On an empty cluster it records
// binaryVersion (the founding node stamps the schema). If the cluster is
// already at or below binaryVersion the node may join — readers tolerate their
// own and older formats. If the cluster is newer, it returns ErrSchemaTooNew
// and the caller must refuse to start.
//
// It never raises the recorded version: a newer binary joining a cluster still
// at an older schema joins at that older schema and keeps writing the old
// format until an explicit migration (RunMigrations) bumps it — so a
// half-upgraded cluster never has a node unilaterally break its peers.
func EnsureCompatible(ctx context.Context, client *clientv3.Client, cfg Config, binaryVersion int) (clusterVersion int, err error) {
	cfg = cfg.withDefaults()
	key := cfg.schemaVersionKey()

	for {
		cur, ok, err := loadSchemaVersion(ctx, client, key)
		if err != nil {
			return 0, err
		}

		if ok {
			if cur > binaryVersion {
				return cur, errors.Wrapf(ErrSchemaTooNew, "cluster at v%d, binary implements v%d", cur, binaryVersion)
			}

			return cur, nil
		}

		// Empty cluster: stamp the schema, but only if still absent — another
		// founding node may race us; on a lost race, re-read.
		resp, err := client.Txn(ctx).
			If(clientv3.Compare(clientv3.CreateRevision(key), "=", 0)).
			Then(clientv3.OpPut(key, strconv.Itoa(binaryVersion))).
			Commit()
		if err != nil {
			return 0, errors.Wrap(err, "stamp schema version")
		}

		if resp.Succeeded {
			return binaryVersion, nil
		}
	}
}

// LoadSchemaVersion returns the cluster's recorded schema version; ok is false
// when no cluster has stamped one yet.
func LoadSchemaVersion(ctx context.Context, client *clientv3.Client, cfg Config) (version int, ok bool, err error) {
	return loadSchemaVersion(ctx, client, cfg.withDefaults().schemaVersionKey())
}

// loadSchemaVersion reads and parses the schema-version key.
func loadSchemaVersion(ctx context.Context, client *clientv3.Client, key string) (version int, ok bool, err error) {
	resp, err := client.Get(ctx, key)
	if err != nil {
		return 0, false, errors.Wrap(err, "read schema version")
	}

	if len(resp.Kvs) == 0 {
		return 0, false, nil
	}

	v, err := strconv.Atoi(string(resp.Kvs[0].Value))
	if err != nil {
		return 0, false, errors.Wrap(err, "parse schema version")
	}

	return v, true, nil
}
