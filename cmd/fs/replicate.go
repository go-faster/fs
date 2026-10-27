package main

import (
	"fmt"
	"io"
	"os"
	"slices"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/go-faster/errors"
	"github.com/spf13/cobra"

	"github.com/go-faster/fs/adminapi"
)

// Replication is the `fs replication` command group: a bucket's
// geo-replication targets and their progress, through a running server's
// admin API.
func Replication() *cobra.Command {
	var addr, token string

	cmd := &cobra.Command{
		Use:   "replication",
		Short: "Show or set a bucket's geo-replication targets",
		Long: `Replicate a bucket's versions to buckets of other fs deployments
(docs/design/geo-replication.md). The bucket and every target bucket need
versioning enabled, and each target needs an access key with the admin grant
on its bucket. Versions written after a target is set ship to it, with their
IDs, times, metadata, tags and ACLs; version deletes and lifecycle actions do
not.`,
	}

	cmd.PersistentFlags().StringVar(&addr, "admin-addr", envOr("FS_ADMIN_ADDR", "http://"+DefaultAdminAddr), "Admin API base URL")
	cmd.PersistentFlags().StringVar(&token, "token", "", "Admin API token (default $FS_ADMIN_TOKEN)")

	cmd.AddCommand(&cobra.Command{
		Use:   "get BUCKET",
		Short: "List a bucket's targets",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := adminClient(addr, token)
			if err != nil {
				return err
			}

			got, err := client.GetBucketReplication(cmd.Context(), adminapi.GetBucketReplicationParams{Bucket: args[0]})
			if err != nil {
				return errors.Wrap(err, "get replication")
			}

			printTargets(cmd.OutOrStdout(), got.Targets)

			return nil
		},
	})

	var (
		t      adminapi.ReplicationTarget
		secret string
	)

	set := &cobra.Command{
		Use:   "set BUCKET",
		Short: "Add a target, or replace the one of the same name",
		Long: `Add a target, or replace the one of the same name; the bucket's other
targets are kept. The server checks the target first: it must answer with
the given credentials and have versioning enabled. Without --secret-key (or
$FS_REPLICATION_SECRET_KEY), a target being replaced keeps its secret.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := adminClient(addr, token)
			if err != nil {
				return err
			}

			ctx, bucket := cmd.Context(), args[0]

			got, err := client.GetBucketReplication(ctx, adminapi.GetBucketReplicationParams{Bucket: bucket})
			if err != nil {
				return errors.Wrap(err, "get replication")
			}

			nt := t
			if secret != "" {
				nt.SecretAccessKey = adminapi.NewOptString(secret)
			}

			if f := cmd.Flags(); f.Changed("prefix") {
				nt.Prefix = adminapi.NewOptString(t.Prefix.Value)
			}

			targets := slices.DeleteFunc(got.Targets, func(o adminapi.ReplicationTarget) bool { return o.Name == nt.Name })
			targets = append(targets, nt)

			got, err = client.SetBucketReplication(ctx, &adminapi.Replication{Targets: targets},
				adminapi.SetBucketReplicationParams{Bucket: bucket})
			if err != nil {
				return errors.Wrap(err, "set replication")
			}

			printTargets(cmd.OutOrStdout(), got.Targets)

			return nil
		},
	}

	f := set.Flags()
	f.StringVar(&t.Name, "name", "", "Target name, unique within the bucket")
	f.StringVar(&t.Endpoint, "endpoint", "", "The target deployment's S3 endpoint, e.g. https://s3.eu.example.com")
	f.StringVar(&t.Bucket, "target-bucket", "", "The target bucket")
	f.StringVar(&t.AccessKeyID, "access-key", "", "Access key with the admin grant on the target bucket")
	f.StringVar(&secret, "secret-key", os.Getenv("FS_REPLICATION_SECRET_KEY"),
		"Its secret (default $FS_REPLICATION_SECRET_KEY, which keeps it out of the process list)")
	f.StringVar(&t.Prefix.Value, "prefix", "", "Replicate only the keys that start with it")
	f.BoolVar(&t.DeleteMarkers.Value, "delete-markers", false, "Replicate delete markers too")
	f.BoolVar(&t.Insecure.Value, "insecure", false, "Allow an http:// endpoint; replication carries data keys")

	for _, name := range []string{"name", "endpoint", "target-bucket", "access-key"} {
		_ = set.MarkFlagRequired(name)
	}

	set.PreRun = func(*cobra.Command, []string) {
		t.DeleteMarkers.Set = t.DeleteMarkers.Value
		t.Insecure.Set = t.Insecure.Value
	}

	cmd.AddCommand(set)

	cmd.AddCommand(&cobra.Command{
		Use:   "rm BUCKET [NAME]",
		Short: "Remove a target, or without NAME every target",
		Long: `Remove a target, or without NAME every target. What has not shipped to a
removed target yet never will.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := adminClient(addr, token)
			if err != nil {
				return err
			}

			ctx, bucket := cmd.Context(), args[0]

			if len(args) == 1 {
				if err := client.DeleteBucketReplication(ctx, adminapi.DeleteBucketReplicationParams{Bucket: bucket}); err != nil {
					return errors.Wrap(err, "delete replication")
				}

				return nil
			}

			got, err := client.GetBucketReplication(ctx, adminapi.GetBucketReplicationParams{Bucket: bucket})
			if err != nil {
				return errors.Wrap(err, "get replication")
			}

			n := len(got.Targets)
			targets := slices.DeleteFunc(got.Targets, func(o adminapi.ReplicationTarget) bool { return o.Name == args[1] })

			if len(targets) == n {
				return errors.Errorf("bucket %q has no target %q", bucket, args[1])
			}

			got, err = client.SetBucketReplication(ctx, &adminapi.Replication{Targets: targets},
				adminapi.SetBucketReplicationParams{Bucket: bucket})
			if err != nil {
				return errors.Wrap(err, "set replication")
			}

			printTargets(cmd.OutOrStdout(), got.Targets)

			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "status BUCKET",
		Short: "Show how far a bucket's replication has got",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := adminClient(addr, token)
			if err != nil {
				return err
			}

			st, err := client.GetBucketReplicationStatus(cmd.Context(), adminapi.GetBucketReplicationStatusParams{Bucket: args[0]})
			if err != nil {
				return errors.Wrap(err, "get replication status")
			}

			printReplicationStatus(cmd.OutOrStdout(), st)

			return nil
		},
	})

	return cmd
}

func printTargets(w io.Writer, targets []adminapi.ReplicationTarget) {
	if len(targets) == 0 {
		_, _ = fmt.Fprintln(w, "no targets")

		return
	}

	for _, t := range targets {
		_, _ = fmt.Fprintf(w, "%s\t%s/%s\tkey %s", t.Name, t.Endpoint, t.Bucket, t.AccessKeyID)

		if p := t.Prefix.Or(""); p != "" {
			_, _ = fmt.Fprintf(w, "\tprefix %q", p)
		}

		if t.DeleteMarkers.Or(false) {
			_, _ = fmt.Fprint(w, "\tdelete markers")
		}

		if t.Insecure.Or(false) {
			_, _ = fmt.Fprint(w, "\tinsecure")
		}

		_, _ = fmt.Fprintln(w)
	}
}

func printReplicationStatus(w io.Writer, st *adminapi.ReplicationStatus) {
	bytes := func(n int64) string { return humanize.IBytes(uint64(max(n, 0))) }

	_, _ = fmt.Fprintf(w, "backlog      %d versions, %s\n", st.Backlog, bytes(st.BacklogBytes))

	if !st.Complete {
		_, _ = fmt.Fprintln(w, "             (outbox read in part: per-target numbers are lower bounds)")
	}

	if !st.Runner {
		_, _ = fmt.Fprintln(w, "runner       another node; ask it for deliveries and errors")
	}

	for _, t := range st.Targets {
		_, _ = fmt.Fprintf(w, "\ntarget       %s\n", t.Name)
		_, _ = fmt.Fprintf(w, "pending      %d versions, %s\n", t.Pending, bytes(t.PendingBytes))
		_, _ = fmt.Fprintf(w, "lag          %s\n", time.Duration(t.LagSeconds*float64(time.Second)).Round(time.Second))
		_, _ = fmt.Fprintf(w, "refused      %d\n", t.Refused)

		if t.Shipped.Set {
			_, _ = fmt.Fprintf(w, "shipped      %d versions; %d of %d blocks sent, %s\n",
				t.Shipped.Value, t.BlocksSent.Or(0), t.BlocksOffered.Or(0), bytes(t.BytesSent.Or(0)))
		}

		if v, ok := t.LastSuccess.Get(); ok {
			_, _ = fmt.Fprintf(w, "last success %s\n", v.UTC().Format(time.RFC3339))
		}

		if t.Failing.Or(false) {
			_, _ = fmt.Fprintf(w, "failing      since %s: %s\n", t.LastFailure.Value.UTC().Format(time.RFC3339), t.LastError.Or(""))
		}

		for _, r := range t.RecentRefusals {
			_, _ = fmt.Fprintf(w, "  refused    %s@%s %s: %s\n", r.Key, r.VersionID, r.At.UTC().Format(time.RFC3339), r.Reason)
		}
	}
}
