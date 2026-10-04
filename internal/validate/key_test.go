package validate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

func TestKey(t *testing.T) {
	// Names S3 stores as they are: a key is never a path.
	for _, k := range []string{
		"a", "file.txt", "dir/sub/file", "/leading", "trailing/", "a//b", "x/../y", "../up", "./here",
		"a/./b", `back\slash`, `C:\drive`, "C:drive", "..hidden", "tab\there", "unicode-café-日本",
		"plus+percent%20", strings.Repeat("k", 1024),
	} {
		assert.NoError(t, Key(k), "%q", k)
	}

	// What the protocol cannot carry.
	for _, k := range []string{
		"", ".", "..", "/", strings.Repeat("k", 1025), "nul\x00", "bell\x07", "del\x7f", "line\nfeed", "\xff\xfe",
	} {
		err := Key(k)
		require.Error(t, err, "%q", k)
		assert.ErrorIs(t, err, fs.ErrInvalidKey)
	}
}
