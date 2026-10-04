package main

import (
	"fmt"

	"github.com/go-faster/errors"
	"github.com/spf13/cobra"

	"github.com/go-faster/fs/adminapi"
)

// Bucket is the `fs bucket` command group: per-bucket settings S3 has no
// call for, through a running server's admin API.
func Bucket() *cobra.Command {
	var addr, token string

	cmd := &cobra.Command{
		Use:   "bucket",
		Short: "Manage per-bucket settings that S3 has no call for",
	}

	cmd.PersistentFlags().StringVar(&addr, "admin-addr", envOr("FS_ADMIN_ADDR", "http://"+DefaultAdminAddr), "Admin API base URL")
	cmd.PersistentFlags().StringVar(&token, "token", "", "Admin API token (default $FS_ADMIN_TOKEN)")

	cmd.AddCommand(&cobra.Command{
		Use:   "scheme BUCKET [SCHEME]",
		Short: "Show or set how a bucket's new blocks are stored",
		Long: `Show or set how a bucket's new blocks are stored: rf3 (three copies, the
default) or ec:K,M (erasure coded as K data and M parity shards, e.g. ec:4,2
for 1.5x the data surviving any two lost).

A coded scheme needs a cluster layout spread for width K+M (fs layout apply
with that width). It applies to blocks written from now on; blocks under
256 KiB and small objects stay replicated.`,
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := adminClient(addr, token)
			if err != nil {
				return err
			}

			var res *adminapi.BucketScheme

			if len(args) == 2 {
				res, err = c.SetBucketScheme(cmd.Context(), &adminapi.BucketScheme{Scheme: args[1]},
					adminapi.SetBucketSchemeParams{Bucket: args[0]})
			} else {
				res, err = c.GetBucketScheme(cmd.Context(), adminapi.GetBucketSchemeParams{Bucket: args[0]})
			}

			if err != nil {
				return errors.Wrap(err, "bucket scheme")
			}

			_, _ = fmt.Fprintln(cmd.OutOrStdout(), res.Scheme)

			return nil
		},
	})

	return cmd
}
