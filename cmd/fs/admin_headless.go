package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/go-faster/errors"
	"github.com/go-faster/sdk/app"
	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/go-faster/fs/clusterstore"
	"github.com/go-faster/fs/internal/adminhandler"
)

// Admin is `fs admin`: a headless, control-plane-only admin. It serves the
// admin API and dashboard for the whole cluster without being an S3 data node
// — reading status from etcd and driving rebalance through the cluster-wide
// election, exactly as any node's admin would. Credential management is
// available here only with cluster-wide credentials (auth.source: etcd), which
// this process manages directly; with file-based auth the access-key endpoints
// return 501 (keys stay per-node).
func Admin() *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:   "admin",
		Short: "Run the cluster-wide admin API and dashboard (headless)",
		Long: `Run the admin API and web dashboard as a standalone control-plane process,
without serving S3 data.

It connects to the cluster's etcd and peers (like the other 'fs cluster'
commands) and exposes the cluster-wide view — schema version, every node's
disks and capacity, placement skew — plus rebalance control (start/pause/
resume via the etcd election). It needs the cluster section of the node config
and an admin token (admin.token or FS_ADMIN_TOKEN).

With cluster-wide credentials (auth.source: etcd) it also manages access keys
for the whole cluster; with file-based auth, manage keys on a data node's admin
API instead.`,
		Example: `  fs admin --config config.yaml`,
		Args:    cobra.NoArgs,
		Run: func(cmd *cobra.Command, _ []string) {
			cfg, err := LoadConfig(configPath)
			if err != nil {
				fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
				os.Exit(1)
			}

			if err := validateClusterClientConfig(cfg); err != nil {
				fmt.Fprintf(os.Stderr, "Error: %v\n", err)
				os.Exit(1)
			}

			// SIGTERM (systemd/k8s stop) → graceful shutdown, same as `fs s3`.
			bridgeSIGTERM()

			start := time.Now()

			app.Run(func(ctx context.Context, lg *zap.Logger, t *app.Telemetry) error {
				return runHeadlessAdmin(ctx, lg, t, cfg, start)
			}, app.WithServiceName(cfg.Observability.ServiceName))
		},
	}

	cmd.Flags().StringVarP(&configPath, "config", "c", "", "Path to YAML configuration file (cluster section)")

	return cmd
}

// runHeadlessAdmin dials the cluster as a disk-less client and serves the admin
// API/dashboard over it.
func runHeadlessAdmin(ctx context.Context, lg *zap.Logger, t *app.Telemetry, cfg Config, start time.Time) error {
	cl, err := dialClusterClient(ctx, cfg, "admin", nil)
	if err != nil {
		return err
	}
	defer func() { _ = cl.Close() }()

	repairer, err := clusterstore.NewRepairer(clusterstore.RepairerConfig{
		Coordinator: cl.coord,
		Self:        cl.self,
		Verify:      true,
		OnError: func(bucket, key string, err error) {
			lg.Warn("Rebalance object failed",
				zap.String("bucket", bucket), zap.String("key", key), zap.Error(err))
		},
	})
	if err != nil {
		return errors.Wrap(err, "cluster repairer")
	}

	// The rebalance runner behind the admin API: the same elected,
	// cursor-checkpointed walk as a node's admin, driven from this disk-less
	// client.
	controller := newRebalanceController(ctx, lg, cl.client, cl.etcdCfg, cl.coord, repairer, string(cl.self))
	status := newClusterStatusSource(cl.coord, cl.client, cl.etcdCfg)

	// With auth.source: etcd, credentials are cluster-wide, so this headless
	// admin manages the very same store every data node watches — key CRUD here
	// propagates to all nodes. With file-based auth there is no cluster
	// credential store to manage from here (each node owns its own), so the
	// access-key endpoints report 501.
	var credentials adminhandler.CredentialManager

	if cfg.AuthSourceValue() == AuthSourceEtcd {
		// nil bootstraps: the headless admin only reads and manages cluster
		// credentials, it never seeds — data nodes own the config that seeds a
		// fresh cluster.
		clusterCreds, err := newClusterCredentials(ctx, lg, cl.client, cl.etcdCfg,
			cfg.ClusterSecret(), nil, nil)
		if err != nil {
			return errors.Wrap(err, "cluster credentials")
		}

		defer func() { _ = clusterCreds.Close() }()

		credentials = clusterCreds
	}

	lg.Info("Starting headless cluster admin", zap.String("candidate", string(cl.self)))

	// authEnabled is reported false — this process is not an S3 server; the data
	// nodes carry their own auth. No reloader either: there is no S3 config to
	// hot-reload, so the reload endpoint reports 501. Per-bucket schemes are
	// cluster-wide, so this control-plane admin serves them through the same
	// coordinator.
	return runAdminServer(t.ShutdownContext(), lg, t, cfg.Admin, credentials, false, start, controller, status, newBucketSchemeSource(cl.coord), clusterDefaultScheme(cfg), nil)
}
