package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildSystemdUnit_User(t *testing.T) {
	unit, err := buildSystemdUnit(true, "/usr/local/bin/fs", "/etc/fs/config.yaml", nil)
	require.NoError(t, err)

	assert.Contains(t, unit, "ExecStart=/usr/local/bin/fs s3 --config /etc/fs/config.yaml")
	assert.Contains(t, unit, "ExecReload=/bin/kill -HUP $MAINPID")
	assert.Contains(t, unit, "WantedBy=default.target")
	// The user unit omits the DynamicUser hardening block (the process runs as
	// the invoking user).
	assert.NotContains(t, unit, "DynamicUser=yes")
}

func TestBuildSystemdUnit_System(t *testing.T) {
	unit, err := buildSystemdUnit(false, "/usr/local/bin/fs", "/etc/fs/config.yaml", nil)
	require.NoError(t, err)

	assert.Contains(t, unit, "WantedBy=multi-user.target")
	assert.Contains(t, unit, "DynamicUser=yes")
	assert.Contains(t, unit, "StateDirectory=fs")
	assert.Contains(t, unit, "ProtectSystem=strict")
}

func TestBuildSystemdUnit_NoConfig(t *testing.T) {
	unit, err := buildSystemdUnit(true, "/usr/local/bin/fs", "", nil)
	require.NoError(t, err)

	assert.Contains(t, unit, "ExecStart=/usr/local/bin/fs s3\n")
	assert.NotContains(t, unit, "--config")
}

func TestBuildSystemdUnit_QuotesSpacedPath(t *testing.T) {
	unit, err := buildSystemdUnit(true, "/opt/my apps/fs", "", nil)
	require.NoError(t, err)

	assert.Contains(t, unit, `ExecStart="/opt/my apps/fs" s3`)
}

func TestBuildSystemdUnit_Environment(t *testing.T) {
	unit, err := buildSystemdUnit(true, "/usr/local/bin/fs", "",
		[]string{"OTEL_TRACES_EXPORTER=none", "METRICS_ADDR=127.0.0.1:8464"})
	require.NoError(t, err)

	assert.Contains(t, unit, "Environment=OTEL_TRACES_EXPORTER=none")
	assert.Contains(t, unit, "Environment=METRICS_ADDR=127.0.0.1:8464")
}

func TestBuildSystemdUnit_InvalidEnv(t *testing.T) {
	_, err := buildSystemdUnit(true, "/usr/local/bin/fs", "", []string{"NOTVALID"})
	require.Error(t, err)
}

func TestInstallUserUnit(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)

	unit, err := buildSystemdUnit(true, "/usr/local/bin/fs", "/etc/fs/config.yaml", nil)
	require.NoError(t, err)

	require.NoError(t, installUserUnit("fs.service", unit))

	path := filepath.Join(dir, "systemd", "user", "fs.service")

	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, unit, string(got))
}
