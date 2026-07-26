package main

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"text/tabwriter"

	"github.com/go-faster/errors"
	"github.com/spf13/cobra"

	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/etcd"
)

// ClusterDrain is `fs cluster drain`: take a disk out of placement, put it
// back, or list what is currently overridden.
func ClusterDrain() *cobra.Command {
	var (
		configPath string
		weight     float64
		reason     string
		undrain    bool
	)

	cmd := &cobra.Command{
		Use:   "drain [<node> <disk>]",
		Short: "Drain a disk out of placement, or list drained disks",
		Long: `Take one disk out of placement without editing a config file or restarting
its node. No new data is placed on it, and the auto-rebalancer moves what it
holds onto the remaining disks; the disk can be removed once it is empty.

The override is control-plane state, not the node's, so it survives the node
restarting — a node republishes its own registration on every capacity refresh,
and a weight written there would be undone within the refresh interval.

--weight sets a partial weight instead of draining, for shifting load off a
disk without emptying it. --undrain clears the override, restoring the weight
the node's config declares.

A disk can be drained while its node is down: the moment an operator most wants
a disk out of placement is often the moment it is unreachable. An override for a
disk that never appears does nothing.`,
		Example: `  # What is currently overridden
  fs cluster drain --config config.yaml

  # Drain a disk before pulling it
  fs cluster drain node-a d1 --reason "failing SMART" --config config.yaml

  # Half the load, without emptying it
  fs cluster drain node-a d1 --weight 0.5 --config config.yaml

  # Put it back
  fs cluster drain node-a d1 --undrain --config config.yaml`,
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()

			cfg, err := LoadConfig(configPath)
			if err != nil {
				return err
			}

			if len(args) == 1 {
				return errors.New("both a node and a disk are required")
			}

			if len(args) == 0 {
				return listDrained(ctx, cmd.OutOrStdout(), cfg)
			}

			if undrain && cmd.Flags().Changed("weight") {
				return errors.New("--undrain and --weight are mutually exclusive")
			}

			return setDrain(ctx, cmd.OutOrStdout(), cfg, args[0], args[1], weight, reason, undrain)
		},
	}

	cmd.Flags().StringVarP(&configPath, "config", "c", "", "Path to YAML configuration file (cluster section)")
	cmd.Flags().Float64Var(&weight, "weight", 0,
		"Placement weight to set; not positive drains the disk (default 0)")
	cmd.Flags().StringVar(&reason, "reason", "", "Free text recorded with the override")
	cmd.Flags().BoolVar(&undrain, "undrain", false, "Clear the override, restoring the configured weight")

	return cmd
}

// listDrained prints every override currently set.
func listDrained(ctx context.Context, out io.Writer, cfg Config) error {
	cl, err := dialClusterClient(ctx, cfg, "drain", nil)
	if err != nil {
		return err
	}

	defer func() { _ = cl.Close() }()

	overrides, err := etcd.ListDiskWeights(ctx, cl.client, cl.etcdCfg)
	if err != nil {
		return err
	}

	if len(overrides) == 0 {
		_, _ = fmt.Fprintln(out, "No disk weight overrides.")

		return nil
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "NODE\tDISK\tWEIGHT\tSTATE\tREASON")

	for _, o := range overrides {
		state := "weighted"
		if o.Drained() {
			state = "drained"
		}

		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n",
			o.Node, o.Disk, strconv.FormatFloat(o.Weight, 'g', -1, 64), state, o.Reason)
	}

	return w.Flush()
}

// setDrain applies or clears one disk's override.
func setDrain(
	ctx context.Context, out io.Writer, cfg Config,
	node, disk string, weight float64, reason string, undrain bool,
) error {
	cl, err := dialClusterClient(ctx, cfg, "drain", nil)
	if err != nil {
		return err
	}

	defer func() { _ = cl.Close() }()

	nodeID, diskID := cluster.NodeID(node), cluster.DiskID(disk)

	if undrain {
		if err := etcd.ClearDiskWeight(ctx, cl.client, cl.etcdCfg, nodeID, diskID); err != nil {
			return err
		}

		_, _ = fmt.Fprintf(out, "Cleared the weight override on %s/%s; it returns to its configured weight.\n",
			node, disk)

		return nil
	}

	if err := etcd.SetDiskWeight(ctx, cl.client, cl.etcdCfg, nodeID, diskID, weight, reason); err != nil {
		return err
	}

	if weight > 0 {
		_, _ = fmt.Fprintf(out, "Set %s/%s to weight %s.\n",
			node, disk, strconv.FormatFloat(weight, 'g', -1, 64))

		return nil
	}

	_, _ = fmt.Fprintf(out,
		"Drained %s/%s. No new data is placed on it; run 'fs cluster rebalance' to move what it holds now,\n"+
			"or let the periodic scrubs converge. Check progress with 'fs cluster status'.\n", node, disk)

	return nil
}
