package auth

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestActionNeeds(t *testing.T) {
	s, err := NewStore(Config{Keys: []Key{
		{AccessKey: "R", SecretKey: "r", Grants: []Grant{{Pattern: "b", Permission: Read}}},
		{AccessKey: "W", SecretKey: "w", Grants: []Grant{{Pattern: "b", Permission: Write}}},
		{AccessKey: "A", SecretKey: "a", Grants: []Grant{{Pattern: "b", Permission: Admin}}},
		{AccessKey: "WA", SecretKey: "wa", Grants: []Grant{{Pattern: "*", Permission: Admin}}},
	}})
	require.NoError(t, err)

	for _, tt := range []struct {
		key    string
		action Action
		want   bool
	}{
		{"R", ActionRead, true},
		{"R", ActionWrite, false},
		{"W", ActionWrite, true},
		{"W", ActionAdmin, false},
		{"A", ActionAdmin, true},
		{"A", ActionWrite, true},
	} {
		assert.Equal(t, tt.want, s.Allow(tt.key, "b", tt.action), "%s %d", tt.key, tt.action)
	}

	assert.True(t, s.AllowsExplicitly("A", "b", ActionAdmin))
	assert.False(t, s.AllowsExplicitly("W", "b", ActionAdmin))
	assert.False(t, s.AllowsExplicitly("WA", "b", ActionAdmin), "a wildcard is not explicit")
}
