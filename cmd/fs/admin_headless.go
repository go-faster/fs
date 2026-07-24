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
)

// Admin is `fs admin`: a headless, control-plane-only admin. It serves the
// admin API and dashboard for the whole cluster without being an S3 data node
// — reading status from etcd and driving rebalance through the cluster-wide
// election, exactly as any node's admin would. Credential management is not
// available here (it stays per-node until cluster-wide credentials land); the
// access-key endpoints return 501.
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

Credential management is not available on this listener; manage access keys on
a data node's admin API.`,
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

	lg.Info("Starting headless cluster admin", zap.String("candidate", string(cl.self)))

	// No local credential store (Manager nil): the access-key endpoints report
	// 501 here. authEnabled is reported false — this process is not an S3
	// server; the data nodes carry their own auth.
	return runAdminServer(t.ShutdownContext(), lg, t, cfg.Admin, nil, false, start, controller, status)
}
