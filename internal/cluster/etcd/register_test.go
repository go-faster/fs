package etcd_test

import (
	"testing"

	"github.com/stretchr/testify/require"
	clientv3 "go.etcd.io/etcd/client/v3"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/etcd"
)

// TestCloseAfterTheLeaseExpired: revoking a lease that is already gone achieved
// what revoking was for.
//
// It happens whenever a node was partitioned or stalled longer than the TTL —
// the registry dropped the node, which is exactly what Close would have done.
// Reporting it as a failure makes a clean shutdown fail for having already
// succeeded, and a node whose shutdown "failed" is one an operator investigates.
func TestCloseAfterTheLeaseExpired(t *testing.T) {
	client := startEtcd(t)
	cfg := etcd.Config{Prefix: "/expired", TTL: 1}

	reg, err := etcd.Register(t.Context(), client, cfg, cluster.Node{
		ID: "n0", Addr: "127.0.0.1:1", Disks: []cluster.Disk{{ID: "d0", Weight: 1}},
	})
	require.NoError(t, err)

	// Revoke it out from under the registration, which is what an expiry looks
	// like from Close's point of view: the lease is simply not there.
	require.NoError(t, revokeUnderneath(t, client, cfg))

	require.NoError(t, reg.Close(), "the lease was already gone, which is the goal")
}

// revokeUnderneath expires every lease behind the registry, without telling the
// Registration.
func revokeUnderneath(t *testing.T, client *clientv3.Client, cfg etcd.Config) error {
	t.Helper()

	resp, err := client.Get(t.Context(), cfg.Prefix+"/nodes/", clientv3.WithPrefix())
	require.NoError(t, err)
	require.NotEmpty(t, resp.Kvs, "the node must be registered before its lease can expire")

	for _, kv := range resp.Kvs {
		if kv.Lease == 0 {
			continue
		}

		if _, err := client.Revoke(t.Context(), clientv3.LeaseID(kv.Lease)); err != nil {
			return err
		}
	}

	return nil
}
