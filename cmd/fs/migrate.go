package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/go-faster/fs/internal/cluster/etcd"
)

// clusterMigrations is the ordered set of schema migrations this binary knows
// how to apply. It is empty at schema v1 (the founding version); each future
// incompatible change to the on-disk or etcd format adds a Migration whose
// Version() is the new etcd.SchemaVersion. cl gives migrations access to the
// cluster (etcd client, coordinator) they need.
func clusterMigrations(_ *clusterClient) []etcd.Migration {
	return nil
}

// ClusterMigrate is `fs cluster migrate`: apply pending schema migrations.
func ClusterMigrate() *cobra.Command {
	var (
		configPath string
		dryRun     bool
	)

	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Apply pending cluster schema migrations",
		Long: `Bring the cluster's on-disk and etcd schema up to the version this binary
implements, applying each pending migration in order.

At most one migrator runs cluster-wide (etcd election) and progress is recorded
after each migration, so a killed run is resumed cleanly. Run this once after a
rolling upgrade has replaced every node's binary — until then the cluster keeps
operating at the old schema, and a node that is too old to understand the
cluster's schema refuses to start.`,
		Example: `  # Show the cluster and binary schema versions and any pending migrations
  fs cluster migrate --config config.yaml --dry-run

  # Apply pending migrations
  fs cluster migrate --config config.yaml`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := LoadConfig(configPath)
			if err != nil {
				return err
			}

			if err := validateClusterClientConfig(cfg); err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			return runMigrate(ctx, cmd.OutOrStdout(), cfg, dryRun)
		},
	}

	cmd.Flags().StringVarP(&configPath, "config", "c", "", "Path to YAML configuration file (cluster section)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Only report the schema versions and pending migrations")

	return cmd
}

// runMigrate reports the schema state and, unless dry-run, applies pending
// migrations under the cluster-wide election.
func runMigrate(ctx context.Context, out io.Writer, cfg Config, dryRun bool) error {
	cl, err := dialClusterClient(ctx, cfg, "migrate", nil)
	if err != nil {
		return err
	}
	defer func() { _ = cl.Close() }()

	clusterVersion, ok, err := etcd.LoadSchemaVersion(ctx, cl.client, cl.etcdCfg)
	if err != nil {
		return err
	}

	if !ok {
		_, _ = fmt.Fprintln(out, "no schema version recorded yet (no node has joined this cluster)")
		return nil
	}

	migrations := clusterMigrations(cl)

	var pending []etcd.Migration

	for _, m := range migrations {
		if m.Version() > clusterVersion && m.Version() <= etcd.SchemaVersion {
			pending = append(pending, m)
		}
	}

	_, _ = fmt.Fprintf(out, "cluster schema: v%d   binary schema: v%d\n", clusterVersion, etcd.SchemaVersion)

	if clusterVersion > etcd.SchemaVersion {
		return etcd.ErrSchemaTooNew
	}

	if len(pending) == 0 {
		_, _ = fmt.Fprintln(out, "schema is up to date; nothing to migrate")
		return nil
	}

	_, _ = fmt.Fprintf(out, "%d pending migration(s):\n", len(pending))
	for _, m := range pending {
		_, _ = fmt.Fprintf(out, "  v%d: %s\n", m.Version(), m.Description())
	}

	if dryRun {
		return nil
	}

	applied, err := etcd.RunMigrations(ctx, cl.client, cl.etcdCfg, etcd.SchemaVersion, string(cl.self), migrations)

	for _, v := range applied {
		_, _ = fmt.Fprintf(out, "applied migration to v%d\n", v)
	}

	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(out, "cluster schema now at v%d\n", etcd.SchemaVersion)

	return nil
}
