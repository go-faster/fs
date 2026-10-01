package reqid_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/go-faster/fs/internal/reqid"
)

func TestNew(t *testing.T) {
	id := reqid.New()

	assert.Regexp(t, `^[0-9A-F]{16}$`, id)
	assert.NotEqual(t, id, reqid.New(), "IDs must not repeat")
}
