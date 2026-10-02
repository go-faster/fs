package engine

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSoloMemberKeepsItsLayout(t *testing.T) {
	dir := t.TempDir()

	m, err := soloMember(dir)
	require.NoError(t, err)

	first := m.Layout()
	require.NotNil(t, first)
	assert.Equal(t, uint64(1), first.Version)
	assert.Len(t, first.Slots, 1)

	again, err := soloMember(dir)
	require.NoError(t, err)
	assert.Equal(t, first, again.Layout(), "a restart keeps the layout it applied, and applies none")
}
