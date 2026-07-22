package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/go-faster/errors"
	"github.com/spf13/cobra"
)

// systemdUnitTemplate renders a systemd service unit for running the S3 server.
// It suits both a per-user service (systemctl --user) and a system service; the
// system variant adds a hardening block and a WantedBy of multi-user.target.
var systemdUnitTemplate = template.Must(template.New("unit").Parse(
	`[Unit]
Description=go-faster/fs S3-compatible storage server
Documentation=https://github.com/go-faster/fs
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart={{ .ExecStart }}
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=5s
{{- if .Environment }}
{{- range .Environment }}
Environment={{ . }}
{{- end }}
{{- end }}
{{- if not .User }}

# Run as a dedicated, unprivileged user (systemd allocates it).
DynamicUser=yes
StateDirectory=fs
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=yes
PrivateTmp=yes
{{- end }}

[Install]
WantedBy={{ .WantedBy }}
`))

// systemdUnitData is the template context.
type systemdUnitData struct {
	ExecStart   string
	Environment []string
	User        bool
	WantedBy    string
}

// Systemd returns the "systemd" command: it generates (and optionally installs)
// a systemd unit for the S3 server.
func Systemd() *cobra.Command {
	var (
		userMode   bool
		install    bool
		configPath string
		execPath   string
		unitName   string
		envs       []string
	)

	cmd := &cobra.Command{
		Use:   "systemd",
		Short: "Generate a systemd service unit for the S3 server",
		Long: `Generate a systemd service unit for running "fs s3".

By default it prints a per-user unit (for "systemctl --user") to stdout. Pass
--install to write it under the systemd user directory and print the commands to
enable it. Use --user=false to generate a hardened system-wide unit instead.`,
		Example: `  # Print a user unit
  fs systemd --config /home/me/fs.yaml

  # Install and enable a user service
  fs systemd --install --config /home/me/fs.yaml
  systemctl --user daemon-reload
  systemctl --user enable --now fs

  # Print a hardened system unit
  fs systemd --user=false --config /etc/fs/config.yaml | sudo tee /etc/systemd/system/fs.service`,
		RunE: func(_ *cobra.Command, _ []string) error {
			data, err := buildSystemdUnit(userMode, execPath, configPath, envs)
			if err != nil {
				return err
			}

			if !install {
				fmt.Print(data)
				return nil
			}

			return installUserUnit(unitName, data)
		},
	}

	cmd.Flags().BoolVar(&userMode, "user", true, "Generate a per-user unit (systemctl --user); false for a system unit")
	cmd.Flags().BoolVar(&install, "install", false, "Install a user unit under the systemd user directory (implies --user)")
	cmd.Flags().StringVarP(&configPath, "config", "c", "", "Config file passed to the service (recommended, absolute path)")
	cmd.Flags().StringVar(&execPath, "exec", "", "Path to the fs binary (defaults to the current executable)")
	cmd.Flags().StringVar(&unitName, "name", "fs.service", "Unit file name to install")
	cmd.Flags().StringArrayVar(&envs, "env", nil, "Environment entry (KEY=VALUE) added to the unit; repeatable")

	return cmd
}

// buildSystemdUnit renders the unit text. envs are validated KEY=VALUE entries
// emitted as Environment= lines.
func buildSystemdUnit(userMode bool, execPath, configPath string, envs []string) (string, error) {
	for _, e := range envs {
		if k, _, ok := strings.Cut(e, "="); !ok || k == "" {
			return "", errors.Errorf("invalid --env %q: want KEY=VALUE", e)
		}
	}

	// A systemd unit is always a Linux artifact: treat paths as opaque POSIX
	// strings and never rewrite them with the host's path rules (filepath.Abs
	// would mangle "/usr/..." into "D:\usr\..." when generating on Windows).
	// Operators pass absolute paths (see the flag help); the os.Executable
	// default below is already absolute.
	exe := execPath
	if exe == "" {
		resolved, err := os.Executable()
		if err != nil {
			// Fall back to a bare name resolved from PATH at runtime.
			exe = "fs"
		} else {
			exe = resolved
		}
	}

	execStart := quoteExec(exe) + " s3"

	if configPath != "" {
		execStart += " --config " + quoteExec(configPath)
	}

	data := systemdUnitData{
		ExecStart:   execStart,
		Environment: envs,
		User:        userMode,
		WantedBy:    "multi-user.target",
	}
	if userMode {
		data.WantedBy = "default.target"
	}

	var sb strings.Builder
	if err := systemdUnitTemplate.Execute(&sb, data); err != nil {
		return "", errors.Wrap(err, "render unit")
	}

	return sb.String(), nil
}

// installUserUnit writes the unit into the systemd user directory and prints the
// commands to enable it.
func installUserUnit(name, unit string) error {
	dir, err := userUnitDir()
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dir, 0o750); err != nil {
		return errors.Wrap(err, "create systemd user dir")
	}

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(unit), 0o644); err != nil { //nolint:gosec // Unit files are world-readable by convention.
		return errors.Wrap(err, "write unit file")
	}

	unitBase := strings.TrimSuffix(name, ".service")

	fmt.Printf("Installed %s\n\nEnable and start it with:\n", path)
	fmt.Println("  systemctl --user daemon-reload")
	fmt.Printf("  systemctl --user enable --now %s\n", unitBase)
	fmt.Println("\nTo keep the service running after you log out:")
	fmt.Println("  loginctl enable-linger $USER")

	return nil
}

// userUnitDir returns the systemd user unit directory, honoring
// XDG_CONFIG_HOME.
func userUnitDir() (string, error) {
	if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
		return filepath.Join(xdg, "systemd", "user"), nil
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return "", errors.Wrap(err, "resolve home directory")
	}

	return filepath.Join(home, ".config", "systemd", "user"), nil
}

// quoteExec wraps a path in double quotes when it contains whitespace, as
// systemd requires for ExecStart arguments.
func quoteExec(p string) string {
	if strings.ContainsAny(p, " \t") {
		return `"` + p + `"`
	}

	return p
}
