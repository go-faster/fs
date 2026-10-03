package main

import (
	"fmt"

	"github.com/go-faster/errors"
	"github.com/spf13/cobra"
)

// Encrypt is the `fs encrypt` command group: operator tasks on the master key.
func Encrypt() *cobra.Command {
	var addr, token string

	cmd := &cobra.Command{
		Use:   "encrypt",
		Short: "Manage server-side encryption at rest",
	}

	cmd.PersistentFlags().StringVar(&addr, "admin-addr", envOr("FS_ADMIN_ADDR", "http://"+DefaultAdminAddr), "Admin API base URL")
	cmd.PersistentFlags().StringVar(&token, "token", "", "Admin API token (default $FS_ADMIN_TOKEN)")

	cmd.AddCommand(&cobra.Command{
		Use:   "rotate",
		Short: "Move every object's data key onto the current master key",
		Long: `Move every encrypted object's data key onto the current master key, through
a running server's admin API.

Each object has its own data key, and only that key — a few dozen bytes of
metadata — is sealed by the master key, so rotation rewrites no object data.
It can be interrupted and run again.

  1. Put the new key in encryption.master_key_file (or FS_MASTER_KEY) and move
     the old one to encryption.previous_key_files, on every node, and restart.
  2. Run this command until it reports 0 remaining. One run covers a cluster.
  3. Remove the old key from previous_key_files.

Removing the old key before step 2 finishes makes objects unreadable, so this
command lists what it could not move and exits non-zero while any remain.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := adminClient(addr, token)
			if err != nil {
				return err
			}

			res, err := c.RotateEncryptionKeys(cmd.Context())
			if err != nil {
				return errors.Wrap(err, "rotate")
			}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "rewrapped %d, already current %d, remaining %d\n", res.Rewrapped, res.Current, res.Remaining)

			for _, f := range res.Failed {
				_, _ = fmt.Fprintln(out, "  not moved:", f)
			}

			if res.Remaining > 0 {
				return errors.Errorf("%d data keys remain under a previous master key; keep it", res.Remaining)
			}

			return nil
		},
	})

	return cmd
}
