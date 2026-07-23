package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/go-faster/fs/internal/cluster/scheme"
)

// ClusterScheme is `fs cluster scheme`: show or change a bucket's replication
// scheme.
func ClusterScheme() *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:   "scheme <bucket> [<scheme>|default]",
		Short: "Show or change a bucket's replication scheme",
		Long: `Show or change the replication scheme of a bucket's objects: "rf2.5"
(default), "rf3" or "ec:k,m" (e.g. "ec:4,2"). "default" clears the override,
restoring the cluster default.

Changing the scheme affects new writes immediately (cluster-wide within a few
seconds). Existing objects are converted by the repair machinery: run
'fs cluster rebalance' to convert everything now, or let the periodic node
scrubs converge the bucket gradually. An object mid-conversion is never below
the stronger of the two schemes' guarantees.`,
		Example: `  # Show the bucket's scheme
  fs cluster scheme photos --config config.yaml

  # Erasure-code the bucket at RS(4,2) and convert its objects now
  fs cluster scheme photos ec:4,2 --config config.yaml
  fs cluster rebalance --config config.yaml

  # Back to the cluster default
  fs cluster scheme photos default --config config.yaml`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := LoadConfig(configPath)
			if err != nil {
				return err
			}

			if err := validateClusterClientConfig(cfg); err != nil {
				return err
			}

			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()

			set := ""
			if len(args) == 2 {
				set = args[1]
			}

			return runScheme(ctx, cmd.OutOrStdout(), cfg, args[0], set, len(args) == 2)
		},
	}

	cmd.Flags().StringVarP(&configPath, "config", "c", "", "Path to YAML configuration file (cluster section)")

	return cmd
}

// runScheme shows or sets a bucket's scheme via a disk-less cluster client.
func runScheme(ctx context.Context, out io.Writer, cfg Config, bucket, set string, doSet bool) error {
	cl, err := dialClusterClient(ctx, cfg, "scheme", nil)
	if err != nil {
		return err
	}
	defer func() { _ = cl.Close() }()

	if doSet {
		if set == "default" {
			set = ""
		}

		if err := cl.coord.SetBucketScheme(ctx, bucket, set); err != nil {
			return err
		}
	}

	info, err := cl.coord.Bucket(ctx, bucket)
	if err != nil {
		return err
	}

	if info.Scheme == "" {
		defaultScheme := scheme.Default

		if cfg.Cluster.Scheme != "" {
			if s, err := scheme.Parse(cfg.Cluster.Scheme); err == nil {
				defaultScheme = s
			}
		}

		_, _ = fmt.Fprintf(out, "bucket %s: cluster default (%s)\n", bucket, defaultScheme)

		return nil
	}

	_, _ = fmt.Fprintf(out, "bucket %s: %s\n", bucket, info.Scheme)

	return nil
}
