package reqid_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/go-faster/fs/internal/reqid"
)

func TestNewIsValid(t *testing.T) {
	id := reqid.New()

	assert.Len(t, id, 16)
	assert.True(t, reqid.Valid(id), "a minted ID must survive its own validator")
	assert.NotEqual(t, id, reqid.New(), "IDs must not repeat")
}

func TestValid(t *testing.T) {
	for name, tc := range map[string]struct {
		id   string
		want bool
	}{
		"minted":        {"55690797AEB458C9", true},
		"lowercase hex": {"55690797aeb458c9", true},
		"max length":    {strings.Repeat("A", reqid.MaxLen), true},
		"dashed uuid":   {"3f2504e0-4f89-11d3-9a0c-0305e82c3301", true},
		"underscore":    {"req_1", true},

		"empty":          {"", false},
		"over max":       {strings.Repeat("A", reqid.MaxLen+1), false},
		"space":          {"55690797 AEB458C9", false},
		"newline":        {"55690797\nAEB458C9", false},
		"tab":            {"5569\t0797", false},
		"nul":            {"5569\x000797", false},
		"terminalEscape": {"\x1b[2J\x1b[H", false},
		"quote":          {`55690797"AEB458C9`, false},
		"non-ascii":      {"55690797Ω", false},
	} {
		t.Run(name, func(t *testing.T) {
			assert.Equal(t, tc.want, reqid.Valid(tc.id))
		})
	}
}

func TestContextRoundTrip(t *testing.T) {
	assert.Empty(t, reqid.FromContext(context.Background()),
		"background work carries no ID")

	ctx := reqid.NewContext(context.Background(), "55690797AEB458C9")
	assert.Equal(t, "55690797AEB458C9", reqid.FromContext(ctx))
}
