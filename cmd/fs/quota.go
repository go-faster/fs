package main

import (
	"context"
	"fmt"
	"io"
	"strconv"

	"github.com/dustin/go-humanize"
	"github.com/go-faster/errors"
	"github.com/spf13/cobra"

	"github.com/go-faster/fs/adminapi"
)

// Quota is the `fs quota` command group: bucket and account quotas, as
// Ceph RGW's `radosgw-admin quota`, through a running server's admin API.
func Quota() *cobra.Command {
	var addr, token string

	cmd := &cobra.Command{
		Use:   "quota",
		Short: "Show or set bucket and account quotas",
		Long: `Show or set the quota of a bucket, or of an account — every bucket one owner
owns, together. A write that would take either past an enabled limit is
refused with QuotaExceeded (403); deletes, and writes that do not grow what is
stored, never are.

Without flags, prints the quota and current usage. Flags change only what they
name: --max-size and --max-objects take -1 for no limit, and a quota does
nothing until --enable.`,
	}

	cmd.PersistentFlags().StringVar(&addr, "admin-addr", envOr("FS_ADMIN_ADDR", "http://"+DefaultAdminAddr), "Admin API base URL")
	cmd.PersistentFlags().StringVar(&token, "token", "", "Admin API token (default $FS_ADMIN_TOKEN)")

	type ops struct {
		get func(ctx context.Context, c *adminapi.Client, name string) (*adminapi.QuotaStatus, error)
		set func(ctx context.Context, c *adminapi.Client, name string, q *adminapi.Quota) (*adminapi.QuotaStatus, error)
	}

	sub := func(use, short string, o ops) *cobra.Command {
		var (
			enable, disable bool
			maxSize         string
			maxObjects      int64
		)

		c := &cobra.Command{
			Use:   use,
			Short: short,
			Args:  cobra.ExactArgs(1),
			RunE: func(cmd *cobra.Command, args []string) error {
				if enable && disable {
					return errors.New("--enable and --disable are exclusive")
				}

				client, err := adminClient(addr, token)
				if err != nil {
					return err
				}

				ctx, name := cmd.Context(), args[0]

				st, err := o.get(ctx, client, name)
				if err != nil {
					return errors.Wrap(err, "get quota")
				}

				flags := cmd.Flags()
				if flags.Changed("enable") || flags.Changed("disable") || flags.Changed("max-size") || flags.Changed("max-objects") {
					q := st.Quota

					switch {
					case enable:
						q.Enabled = true
					case disable:
						q.Enabled = false
					}

					if flags.Changed("max-size") {
						n, err := parseLimit(maxSize)
						if err != nil {
							return err
						}

						q.MaxSize = adminapi.NewOptInt64(n)
					}

					if flags.Changed("max-objects") {
						if maxObjects < -1 {
							return errors.New("--max-objects is -1 for no limit, or at least 0")
						}

						q.MaxObjects = adminapi.NewOptInt64(maxObjects)
					}

					if st, err = o.set(ctx, client, name, &q); err != nil {
						return errors.Wrap(err, "set quota")
					}
				}

				printQuota(cmd.OutOrStdout(), st)

				return nil
			},
		}

		c.Flags().BoolVar(&enable, "enable", false, "Enforce the quota")
		c.Flags().BoolVar(&disable, "disable", false, "Stop enforcing the quota, keeping its limits")
		c.Flags().StringVar(&maxSize, "max-size", "", "Bytes that may be stored, e.g. 10GiB or 500G; -1 for no limit")
		c.Flags().Int64Var(&maxObjects, "max-objects", -1, "Objects that may be stored; -1 for no limit")

		return c
	}

	cmd.AddCommand(sub("bucket BUCKET", "Show or set a bucket's quota", ops{
		get: func(ctx context.Context, c *adminapi.Client, name string) (*adminapi.QuotaStatus, error) {
			return c.GetBucketQuota(ctx, adminapi.GetBucketQuotaParams{Bucket: name})
		},
		set: func(ctx context.Context, c *adminapi.Client, name string, q *adminapi.Quota) (*adminapi.QuotaStatus, error) {
			return c.SetBucketQuota(ctx, q, adminapi.SetBucketQuotaParams{Bucket: name})
		},
	}))

	cmd.AddCommand(sub("account ACCOUNT", "Show or set an account's quota (ACCOUNT is the owner's user ID)", ops{
		get: func(ctx context.Context, c *adminapi.Client, name string) (*adminapi.QuotaStatus, error) {
			return c.GetAccountQuota(ctx, adminapi.GetAccountQuotaParams{Account: name})
		},
		set: func(ctx context.Context, c *adminapi.Client, name string, q *adminapi.Quota) (*adminapi.QuotaStatus, error) {
			return c.SetAccountQuota(ctx, q, adminapi.SetAccountQuotaParams{Account: name})
		},
	}))

	return cmd
}

// parseLimit reads a byte limit: -1, or a size humanize understands.
func parseLimit(s string) (int64, error) {
	if s == "-1" {
		return -1, nil
	}

	n, err := humanize.ParseBytes(s)
	if err != nil {
		return 0, errors.Wrap(err, "--max-size")
	}

	if n > 1<<62 {
		return 0, errors.Errorf("--max-size %s is too large", s)
	}

	return int64(n), nil
}

func printQuota(w io.Writer, st *adminapi.QuotaStatus) {
	limit := func(v adminapi.OptInt64, format func(int64) string) string {
		if n := v.Or(-1); n >= 0 {
			return format(n)
		}

		return "unlimited"
	}

	size := func(n int64) string {
		return fmt.Sprintf("%s (%d bytes)", humanize.IBytes(uint64(n)), n) //nolint:gosec // Not negative.
	}

	count := func(n int64) string { return strconv.FormatInt(n, 10) }

	_, _ = fmt.Fprintf(w, "enabled      %t\n", st.Quota.Enabled)
	_, _ = fmt.Fprintf(w, "max size     %s\n", limit(st.Quota.MaxSize, size))
	_, _ = fmt.Fprintf(w, "max objects  %s\n", limit(st.Quota.MaxObjects, count))
	_, _ = fmt.Fprintf(w, "usage        %d objects, %s\n", st.Usage.Objects, size(max(st.Usage.Bytes, 0)))
}
