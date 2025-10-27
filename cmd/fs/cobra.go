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
		Short: "go-faster file system utilities",
	}
	return cmd
}

func main() {
	if err := Root().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %+v\n", err)
		os.Exit(1)
	}
}
