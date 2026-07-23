package diskstore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster"
)

func TestUsage(t *testing.T) {
	s, err := New(map[cluster.DiskID]string{"d0": t.TempDir()})
	require.NoError(t, err)

	u, err := s.Usage("d0")
	require.NoError(t, err)
	assert.Positive(t, u.TotalBytes)
	assert.LessOrEqual(t, u.FreeBytes, u.TotalBytes)

	_, err = s.Usage("nope")
	require.Error(t, err)
}
