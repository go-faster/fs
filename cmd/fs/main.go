package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func Root() *cobra.Command {
	cmd := &cobra.Command{
		SilenceErrors: true,
		SilenceUsage:  true,

		Use:   "fs",
		Short: "go-faster/fs",
	}

	// Add subcommands
	cmd.AddCommand(S3())

	return cmd
}

func main() {
	if err := Root().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %+v\n", err)
		os.Exit(1)
	}
}
