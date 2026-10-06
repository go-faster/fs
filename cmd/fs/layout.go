package main

import (
	"bytes"
	"cmp"
	"fmt"
	"io"
	"net/http"
	"os"
	"text/tabwriter"

	"github.com/dustin/go-humanize"
	"github.com/go-faster/errors"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/go-faster/fs/adminapi"
)

// Layout is the `fs layout` command: inspect and change the cluster layout
// through a node's admin API.
func Layout() *cobra.Command {
	var addr, token string

	cmd := &cobra.Command{
		Use:   "layout",
		Short: "Inspect and change the cluster layout",
		Long: `Inspect and change the cluster layout through a node's admin API.

The admin address and token default to the FS_ADMIN_ADDR and FS_ADMIN_TOKEN
environment variables. A layout applied on any node reaches the rest by
gossip.`,
	}

	cmd.PersistentFlags().StringVar(&addr, "admin-addr", envOr("FS_ADMIN_ADDR", "http://"+DefaultAdminAddr), "Admin API base URL")
	cmd.PersistentFlags().StringVar(&token, "token", "", "Admin API token (default $FS_ADMIN_TOKEN)")

	client := func() (*adminapi.Client, error) { return adminClient(addr, token) }

	cmd.AddCommand(&cobra.Command{
		Use:   "show",
		Short: "Show the adopted layout",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}

			l, err := c.GetLayout(cmd.Context())
			if err != nil {
				return errors.Wrap(err, "get layout")
			}

			printLayout(cmd.OutOrStdout(), l)

			return nil
		},
	})

	cmd.AddCommand(&cobra.Command{
		Use:   "nodes",
		Short: "List this node and its peers",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := client()
			if err != nil {
				return err
			}

			list, err := c.ListClusterNodes(cmd.Context())
			if err != nil {
				return errors.Wrap(err, "list nodes")
			}

			printNodes(cmd.OutOrStdout(), list.Nodes)

			return nil
		},
	})

	var (
		file   string
		dryRun bool
	)

	apply := &cobra.Command{
		Use:   "apply -f roles.yaml",
		Short: "Apply the member roles in a file",
		Long: `Compute the next layout from the full set of member roles in a file and
apply it. Slots keep their node wherever it is still valid, so only the data
that has to move does; --dry-run shows how much without applying.

  widths: [3, 6]     # optional: 3 for replicated data, k+m per erasure scheme
  partitions: 256    # optional, first layout only
  members:
    - id: node-1
      zone: dc1
      rack: r1
      capacity: 4TB  # 0 for a gateway that holds no data`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			req, err := readRoles(file)
			if err != nil {
				return err
			}

			c, err := client()
			if err != nil {
				return err
			}

			change, err := c.ApplyLayout(cmd.Context(), req, adminapi.ApplyLayoutParams{DryRun: adminapi.NewOptBool(dryRun)})
			if err != nil {
				return errors.Wrap(err, "apply layout")
			}

			out := cmd.OutOrStdout()
			printLayout(out, &change.Layout)

			verb := "Applied"
			if !change.Applied {
				verb = "Dry run, not applied"
			}

			_, _ = fmt.Fprintf(out, "\n%s: version %d, %d slots move.\n", verb, change.Layout.Version, change.MovedSlots)

			return nil
		},
	}
	apply.Flags().StringVarP(&file, "file", "f", "", "Member roles file (- for stdin)")
	apply.Flags().BoolVar(&dryRun, "dry-run", false, "Show the change without applying it")
	_ = apply.MarkFlagRequired("file")

	cmd.AddCommand(apply)

	cmd.AddCommand(&cobra.Command{
		Use:   "skip NODE",
		Short: "Stop waiting for a node that is gone to sync a layout change",
		Long: `A layout change completes once every node holding data has synced it; until
then the versions before it stay in use. A node that is gone for good never
syncs: skip releases it, so the change can complete. Whatever only that node
held is given up — use it for a node that will not come back.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := client()
			if err != nil {
				return err
			}

			if err := c.SkipClusterNode(cmd.Context(), adminapi.SkipClusterNodeParams{ID: args[0]}); err != nil {
				return errors.Wrap(err, "skip")
			}

			_, _ = fmt.Fprintf(cmd.OutOrStdout(), "%s released; gossip carries it to every node\n", args[0])

			return nil
		},
	})

	return cmd
}

// rolesFile is the shape of `fs layout apply -f`.
type rolesFile struct {
	Widths     []int `yaml:"widths"`
	Partitions int   `yaml:"partitions"`
	Members    []struct {
		ID       string `yaml:"id"`
		Zone     string `yaml:"zone"`
		Rack     string `yaml:"rack"`
		Capacity string `yaml:"capacity"`
	} `yaml:"members"`
}

func readRoles(path string) (*adminapi.ApplyLayoutRequest, error) {
	var (
		b   []byte
		err error
	)

	if path == "-" {
		b, err = io.ReadAll(os.Stdin)
	} else {
		b, err = os.ReadFile(path) // #nosec G304 -- the operator's own file
	}

	if err != nil {
		return nil, errors.Wrap(err, "read roles")
	}

	var f rolesFile

	dec := yaml.NewDecoder(bytes.NewReader(b))
	dec.KnownFields(true)

	if err := dec.Decode(&f); err != nil {
		return nil, errors.Wrap(err, "parse roles")
	}

	req := &adminapi.ApplyLayoutRequest{Widths: f.Widths}
	if f.Partitions > 0 {
		req.Partitions = adminapi.NewOptInt(f.Partitions)
	}

	for _, m := range f.Members {
		capacity, err := humanize.ParseBytes(m.Capacity)
		if err != nil {
			return nil, errors.Wrapf(err, "member %q capacity", m.ID)
		}

		role := adminapi.LayoutRole{ID: m.ID, Capacity: capacity}
		if m.Zone != "" {
			role.Zone = adminapi.NewOptString(m.Zone)
		}

		if m.Rack != "" {
			role.Rack = adminapi.NewOptString(m.Rack)
		}

		req.Members = append(req.Members, role)
	}

	return req, nil
}

func printLayout(w io.Writer, l *adminapi.Layout) {
	_, _ = fmt.Fprintf(w, "Version %d, %d partitions, widths %v\n", l.Version, l.Partitions, l.Widths)

	if len(l.RetainedVersions) > 0 {
		_, _ = fmt.Fprintf(w, "In transition: versions %v still in use until every node syncs %d (see fs layout nodes)\n",
			l.RetainedVersions, l.Version)
	}

	_, _ = fmt.Fprintln(w)

	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tZONE\tRACK\tCAPACITY\tSLOTS")

	for _, m := range l.Members {
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n",
			m.ID, m.Zone.Or("-"), m.Rack.Or("-"), humanize.IBytes(m.Capacity), m.Slots)
	}

	_ = tw.Flush()

	_, _ = fmt.Fprintln(w)

	tw = tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "WIDTH\tMAX PER ZONE\tMAX PER RACK")

	for _, s := range l.Spread {
		_, _ = fmt.Fprintf(tw, "%d\t%d\t%d\n", s.Width, s.MaxPerZone, s.MaxPerRack)
	}

	_ = tw.Flush()
}

func printNodes(w io.Writer, nodes []adminapi.ClusterNode) {
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tADDR\tSTATE\tLAYOUT\tSYNCED\tLAST SEEN\tERROR")

	for _, n := range nodes {
		state := "down"
		if n.Up {
			state = "up"
		}

		if n.Self {
			state = "self"
		}

		seen := "-"
		if v, ok := n.LastSeen.Get(); ok {
			seen = humanize.Time(v)
		}

		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%s\t%s\n",
			n.ID.Or("?"), n.Addr, state, n.LayoutVersion.Or(0), n.SyncedVersion.Or(0), seen, n.Error.Or(""))
	}

	_ = tw.Flush()
}

// bearerTransport adds the admin bearer token to every request.
type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (t bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+t.token)

	return t.base.RoundTrip(r)
}

// adminClient returns an admin API client for addr, authenticating with token
// or, when empty, $FS_ADMIN_TOKEN.
func adminClient(addr, token string) (*adminapi.Client, error) {
	t := cmp.Or(token, os.Getenv(envAdminToken))

	if t == "" {
		return nil, errors.Errorf("no admin token: set --token or %s", envAdminToken)
	}

	return adminapi.NewClient(addr, adminapi.WithClient(&http.Client{
		Transport: bearerTransport{token: t, base: http.DefaultTransport},
	}))
}
