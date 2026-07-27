package main

import (
	"fmt"
	"path/filepath"

	"github.com/go-faster/errors"
	"github.com/spf13/cobra"

	"github.com/go-faster/fs/storagefs"
)

// Encrypt is the `fs encrypt` command group: operator tasks on the master key.
func Encrypt() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "encrypt",
		Short: "Manage server-side encryption at rest",
	}

	cmd.AddCommand(encryptRotate())

	return cmd
}

// encryptRotate is `fs encrypt rotate`.
func encryptRotate() *cobra.Command {
	var configPath string

	cmd := &cobra.Command{
		Use:   "rotate",
		Short: "Move every object's data key onto the current master key",
		Long: `Move every encrypted object's data key onto the current master key.

Object bodies are never read or rewritten: each object has its own data key,
and only that key — a few dozen bytes in the object's metadata — is wrapped by
the master key. Rotating therefore costs a metadata walk rather than
re-encrypting the store, and can be interrupted and resumed.

The order of operations is:

  1. Put the new key in encryption.master_key_file (or FS_MASTER_KEY) and move
     the old one to encryption.previous_key_files.
  2. Run this command until it reports no failures.
  3. Remove the old key from previous_key_files.

Removing the old key before step 2 finishes is what makes objects unreadable,
so this command names every object it could not rewrap and exits non-zero
while any remain.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := LoadConfig(configPath)
			if err != nil {
				return errors.Wrap(err, "load config")
			}

			if cfg.Storage.Type != StorageTypeFilesystem {
				return errors.Errorf(
					"rotation is implemented for storage.type %q; this config uses %q",
					StorageTypeFilesystem, cfg.Storage.Type)
			}

			keyring, err := cfg.Encryption.Keyring()
			if err != nil {
				return errors.Wrap(err, "server-side encryption")
			}

			if keyring == nil {
				return errors.New(
					"no master key is configured; set encryption.master_key_file or " + masterKeyEnv)
			}

			absRoot, err := filepath.Abs(cfg.Storage.Root)
			if err != nil {
				return errors.Wrap(err, "resolve storage root")
			}

			store, err := storagefs.New(absRoot, storagefs.WithEncryption(keyring))
			if err != nil {
				return errors.Wrap(err, "open storage")
			}

			report, err := store.RotateKeys(cmd.Context())
			if err != nil {
				return errors.Wrap(err, "rotate")
			}

			out := cmd.OutOrStdout()
			_, _ = fmt.Fprintf(out, "scanned %d objects\n", report.Scanned)
			_, _ = fmt.Fprintf(out, "  rewrapped:       %d\n", report.Rewrapped)
			_, _ = fmt.Fprintf(out, "  already current: %d\n", report.AlreadyCurrent)
			_, _ = fmt.Fprintf(out, "  unencrypted:     %d\n", report.Unencrypted)

			if !report.Done() {
				for _, ref := range report.Failed {
					_, _ = fmt.Fprintf(out, "  FAILED: %s/%s\n", ref.Bucket, ref.Key)
				}

				// Non-zero, because the operator's next step is to remove the
				// retired key, and doing that now would lose these objects.
				return errors.Errorf(
					"%d objects could not be rewrapped; keep the retired master key configured",
					len(report.Failed))
			}

			_, _ = fmt.Fprintln(out, "rotation complete: every object is on the current master key")

			return nil
		},
	}

	cmd.Flags().StringVarP(&configPath, "config", "c", "", "path to the server config")

	return cmd
}
