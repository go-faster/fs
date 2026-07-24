package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"slices"
	"syscall"

	"github.com/go-faster/errors"
	"github.com/spf13/cobra"
	"golang.org/x/time/rate"

	"github.com/go-faster/fs/clusterstore"
	"github.com/go-faster/fs/internal/cluster"
	"github.com/go-faster/fs/internal/cluster/etcd"
)

// Cluster groups cluster operations.
func Cluster() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "cluster",
		Short: "Cluster operations",
	}

	cmd.AddCommand(ClusterRebalance())
	cmd.AddCommand(ClusterScheme())
	cmd.AddCommand(ClusterMigrate())

	return cmd
}

// rebalanceParams are the resolved flags of `fs cluster rebalance`.
type rebalanceParams struct {
	dryRun      bool
	concurrency int
	rateMiB     float64
	verify      bool
}

// ClusterRebalance is `fs cluster rebalance`: relocate every object to the
// current topology's placement.
func ClusterRebalance() *cobra.Command {
	var (
		configPath string
		params     rebalanceParams
	)

	cmd := &cobra.Command{
		Use:   "rebalance",
		Short: "Relocate objects to the current topology's placement",
		Long: `Walk every object in the cluster and restore it at the current topology's
placement: fragments still sitting at a previous placement are copied to their
new targets, verified, and only then retired (an object is never below its
protection level mid-move).

At most one rebalance runs cluster-wide: runners campaign for an etcd election
and standbys block until the leader exits. Progress is checkpointed to etcd,
so a killed runner is resumed by the next one without restarting the walk.

The command needs the cluster section of the node config (etcd endpoints and
the cluster secret); it talks to the nodes directly and can run from any host
that reaches them.`,
		Example: `  # Show what would move, per node, without touching anything
  fs cluster rebalance --config config.yaml --dry-run

  # Rebalance with 8 parallel objects, capped at 100 MiB/s
  fs cluster rebalance --config config.yaml --concurrency 8 --rate 100`,
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

			return runRebalance(ctx, cmd.OutOrStdout(), cfg, params)
		},
	}

	cmd.Flags().StringVarP(&configPath, "config", "c", "", "Path to YAML configuration file (cluster section)")
	cmd.Flags().BoolVar(&params.dryRun, "dry-run", false, "Only compute and print the move plan (objects/bytes per node)")
	cmd.Flags().IntVar(&params.concurrency, "concurrency", 4, "Objects repaired in parallel")
	cmd.Flags().Float64Var(&params.rateMiB, "rate", 0, "Data movement bandwidth cap in MiB/s (0 = unlimited)")
	cmd.Flags().BoolVar(&params.verify, "verify", true, "Checksum-verify replica payloads while rebalancing")

	return cmd
}

// runRebalance builds a disk-less cluster client and runs the plan or the
// elected rebalance pass.
func runRebalance(ctx context.Context, out io.Writer, cfg Config, params rebalanceParams) error {
	// The runner is a pure client moving data between the nodes; --rate wraps
	// its peer dialer with the bandwidth cap.
	var wrap func(clusterstore.PeerDialer) clusterstore.PeerDialer

	if params.rateMiB > 0 {
		bytesPerSec := params.rateMiB * float64(1<<20)
		wrap = func(d clusterstore.PeerDialer) clusterstore.PeerDialer {
			return &clusterstore.ThrottledPeers{
				Dialer: d,
				// One second of burst keeps large reads smooth at the cap.
				Limiter: rate.NewLimiter(rate.Limit(bytesPerSec), int(bytesPerSec)),
			}
		}
	}

	cl, err := dialClusterClient(ctx, cfg, "rebalance", wrap)
	if err != nil {
		return err
	}
	defer func() { _ = cl.Close() }()

	client, etcdCfg, self := cl.client, cl.etcdCfg, cl.self

	repairer, err := clusterstore.NewRepairer(clusterstore.RepairerConfig{
		Coordinator: cl.coord,
		Self:        self,
		Verify:      params.verify,
		OnError: func(bucket, key string, err error) {
			_, _ = fmt.Fprintf(out, "FAILED %s/%s: %v\n", bucket, key, err)
		},
	})
	if err != nil {
		return errors.Wrap(err, "cluster repairer")
	}

	if params.dryRun {
		plan, err := repairer.PlanRebalance(ctx)
		if err != nil {
			return err
		}

		printRebalancePlan(out, plan)

		return nil
	}

	_, _ = fmt.Fprintf(out, "campaigning for rebalance leadership as %s...\n", self)

	lead, err := etcd.CampaignRebalance(ctx, client, etcdCfg, string(self))
	if err != nil {
		return err
	}
	defer func() { _ = lead.Close() }()

	// Leadership loss (lease expiry under partition) must stop the walk: a
	// standby is about to take over from the last checkpoint.
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	go func() {
		select {
		case <-lead.Done():
			cancel()
		case <-runCtx.Done():
		}
	}()

	var resume clusterstore.RebalanceCursor

	if raw, ok, err := etcd.LoadRebalanceCursor(ctx, client, etcdCfg); err != nil {
		return err
	} else if ok {
		if resume, err = clusterstore.DecodeRebalanceCursor(raw); err != nil {
			return err
		}

		_, _ = fmt.Fprintf(out, "resuming after %s/%s\n", resume.Bucket, resume.Key)
	}

	// Captured before the walk: a mid-run membership change must read as
	// "another walk needed".
	signature := cl.coord.Topology().Signature()

	report, runErr := repairer.Rebalance(runCtx, clusterstore.RebalanceOptions{
		Resume:      resume,
		Concurrency: params.concurrency,
		Checkpoint: func(ctx context.Context, cur clusterstore.RebalanceCursor) error {
			raw, err := cur.Encode()
			if err != nil {
				return err
			}

			return lead.SaveCursor(ctx, raw)
		},
	})
	if runErr == nil {
		// The walk completed: clear the cursor so the next rebalance starts
		// fresh, and record the covered topology signature for the
		// auto-rebalancer.
		if err := lead.ClearCursor(ctx); err != nil {
			_, _ = fmt.Fprintf(out, "warning: %v\n", err)
		}

		if err := lead.SaveApplied(ctx, signature); err != nil {
			_, _ = fmt.Fprintf(out, "warning: %v\n", err)
		}
	}

	_, _ = fmt.Fprintf(out, "rebalance: %d buckets, %d objects, %d relocated, %d failed, %d fragments rebuilt, %d stale copies retired\n",
		report.Buckets, report.Objects, report.Relocated, report.Failed,
		report.Totals.RebuiltFragments, report.Totals.DeletedStale)

	if runErr != nil {
		return errors.Wrap(runErr, "rebalance interrupted (progress is checkpointed; rerun to resume)")
	}

	if report.Failed > 0 {
		return errors.Errorf("%d objects failed to rebalance", report.Failed)
	}

	return nil
}

// printRebalancePlan renders the dry-run plan.
func printRebalancePlan(out io.Writer, plan *clusterstore.RebalancePlan) {
	_, _ = fmt.Fprintf(out, "objects examined:  %d\n", plan.Objects)
	_, _ = fmt.Fprintf(out, "objects to move:   %d\n", plan.MisplacedObjects)
	_, _ = fmt.Fprintf(out, "bytes to move:     %s\n", formatBytes(plan.MisplacedBytes))

	if plan.Unplannable > 0 {
		_, _ = fmt.Fprintf(out, "unplannable:       %d (topology cannot host their scheme)\n", plan.Unplannable)
	}

	if len(plan.Nodes) == 0 {
		_, _ = fmt.Fprintln(out, "nothing to move: every object is at its current placement")
		return
	}

	nodes := make([]cluster.NodeID, 0, len(plan.Nodes))
	for id := range plan.Nodes {
		nodes = append(nodes, id)
	}

	slices.Sort(nodes)

	_, _ = fmt.Fprintf(out, "\n%-24s %10s %12s\n", "NODE", "OBJECTS", "BYTES")

	for _, id := range nodes {
		np := plan.Nodes[id]
		_, _ = fmt.Fprintf(out, "%-24s %10d %12s\n", id, np.Objects, formatBytes(np.Bytes))
	}
}

// formatBytes renders a byte count in binary units.
func formatBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	div, exp := int64(unit), 0
	for u := n / unit; u >= unit; u /= unit {
		div *= unit
		exp++
	}

	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
