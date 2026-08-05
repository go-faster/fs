package main

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/go-faster/fs/internal/cluster/metastore"
)

// TestRebuildAfterAFailureNeedsNoOperator is the case the whole runner exists
// for. A range with no copy of its data leaves the plane degraded now, every
// listing in the cluster on the slow path, and nothing about waiting improves
// it.
func TestRebuildAfterAFailureNeedsNoOperator(t *testing.T) {
	assert.True(t, rebuildWanted(RebuildOnFailure, metastore.CauseOrphaned))
	assert.True(t, rebuildWanted(RebuildAlways, metastore.CauseOrphaned))
}

// TestEnablingThePlaneWaitsForAnOperator: switching the plane on over a cluster
// that already holds objects is planned. An operator chose the moment, and the
// walk of every disk that follows is hours of I/O competing with serving
// traffic — starting it the instant the config lands takes the decision away
// from them and picks the worst moment there is, right after a change.
func TestEnablingThePlaneWaitsForAnOperator(t *testing.T) {
	assert.False(t, rebuildWanted(RebuildOnFailure, metastore.CauseNeverBuilt),
		"a config change started hours of I/O by itself")
	assert.True(t, rebuildWanted(RebuildAlways, metastore.CauseNeverBuilt),
		"and a cluster small enough not to care can say so")
}

// TestNeverMeansNever: an operator who has taken the decision keeps it, even
// for the case that is degraded now.
func TestNeverMeansNever(t *testing.T) {
	for _, cause := range []metastore.BuildCause{
		metastore.CauseOrphaned,
		metastore.CauseNeverBuilt,
		metastore.CauseUnspecified,
	} {
		assert.False(t, rebuildWanted(RebuildNever, cause), "cause %s", cause)
	}
}

// TestAnUnrecordedCauseIsNeverActedOn, whatever the policy — including
// "always".
//
// Unspecified is what the plane reads as when its flag cannot be read, when an
// older cluster wrote it, and when someone started a rebuild by hand. None of
// those is a statement that a fresh walk of every disk is wanted now, and a
// rebuild is far too expensive to start on a guess.
func TestAnUnrecordedCauseIsNeverActedOn(t *testing.T) {
	for _, policy := range []string{RebuildOnFailure, RebuildAlways, RebuildNever} {
		assert.False(t, rebuildWanted(policy, metastore.CauseUnspecified), "policy %s", policy)
	}
}

// TestTheDefaultPolicyIsOnFailure: an unset knob must be the one that fixes a
// degraded cluster and leaves a planned one alone.
func TestTheDefaultPolicyIsOnFailure(t *testing.T) {
	var cfg Config

	assert.Equal(t, RebuildOnFailure, cfg.MetadataRebuild())
}

// TestRebuildPolicyIsValidated: a typo in the one knob that decides whether a
// cluster starts hours of I/O by itself must fail at startup, not silently read
// as some default.
func TestRebuildPolicyIsValidated(t *testing.T) {
	t.Setenv("FS_CLUSTER_NODE_ID", "n0")
	t.Setenv("FS_CLUSTER_ADVERTISE_ADDR", "n0.fs:7080")

	cfg := DefaultConfig()
	cfg.Storage.Type = StorageTypeCluster
	cfg.Cluster = ClusterConfig{
		Secret: "0123456789abcdef0123456789abcdef",
		Etcd:   EtcdConfig{Endpoints: []string{"http://127.0.0.1:2379"}},
	}
	cfg.Cluster.Metadata.Rebuild = "sometimes"

	err := cfg.Validate()
	assert.ErrorContains(t, err, "cluster.metadata.rebuild")

	for _, policy := range []string{"", RebuildOnFailure, RebuildAlways, RebuildNever} {
		cfg.Cluster.Metadata.Rebuild = policy
		assert.NoError(t, cfg.Validate(), "policy %q", policy)
	}
}
