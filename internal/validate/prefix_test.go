package validate

import (
	"strings"
	"testing"

	"github.com/go-faster/errors"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs"
)

// A prefix is a filter, not a path: backends compare it against stored keys
// with strings.HasPrefix and never resolve it on disk. S3 therefore accepts any
// well-formed UTF-8 sequence within the key-length limit — including bytes that
// would be rejected in a key — and returns an empty result set when nothing
// matches. These tests pin that contract.
func TestPrefixAccepts(t *testing.T) {
	for _, tt := range []struct {
		name   string
		prefix string
	}{
		{"empty lists everything", ""},
		{"simple", "folder"},
		{"trailing slash", "folder/"},
		{"nested", "folder/subfolder/"},
		{"leading slash", "/folder"},
		{"single character", "a"},
		{"digits", "2024/01/"},
		{"unicode", "文件夹/"},
		{"emoji", "📁/"},
		{"spaces", "my folder/"},
		{"query-like characters", "a=b&c=d"},
		{"percent", "100%/"},
		{"hash", "1999#"},
		{"plus", "1999+"},
		{"tilde and dashes", "~my-file_name.txt"},
		{"max length", strings.Repeat("a", maxKeyLen)},

		// Rejected in a key, accepted in a prefix: none of these can escape a
		// bucket because the prefix is never joined to a filesystem path.
		{"newline", "\n"},
		{"carriage return", "folder\r/"},
		{"tab", "folder\t/"},
		{"bell", "\a"},
		{"DEL", "\x7f"},
		{"null byte", "folder\x00/"},
		{"dot segment", "./"},
		{"parent segment", "../"},
		{"embedded parent segment", "a/../b"},
		{"embedded dot segment", "a/./b"},
		{"backslash", "folder\\sub"},
		{"windows drive letter", "c:/folder"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			require.NoError(t, Prefix(tt.prefix))
		})
	}
}

func TestPrefixRejects(t *testing.T) {
	for _, tt := range []struct {
		name   string
		prefix string
	}{
		{"over max length", strings.Repeat("a", maxKeyLen+1)},
		{"invalid UTF-8", "\xff\xfe"},
		{"truncated UTF-8 sequence", "folder/\xc3"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := Prefix(tt.prefix)
			require.Error(t, err)
			require.True(t, errors.Is(err, fs.ErrInvalidKey),
				"prefix errors must carry fs.ErrInvalidKey so s3err maps them to 400, got %v", err)
		})
	}
}

// TestPrefixMoreLenientThanKey documents the deliberate asymmetry: every
// traversal-shaped string that Key rejects is fine as a prefix.
func TestPrefixMoreLenientThanKey(t *testing.T) {
	for _, p := range []string{"../", "a/./b", "folder\\sub", "c:/folder", "folder\x00/", "\n"} {
		require.Error(t, Key(p), "Key must still reject %q", p)
		require.NoError(t, Prefix(p), "Prefix must accept %q", p)
	}
}
