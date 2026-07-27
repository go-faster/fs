package storagefs

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestObjectRelPath covers the key-to-path mapping, including the key shapes a
// plain key-as-path mapping cannot represent at all.
func TestObjectRelPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		key      string
		expected []string // path segments, joined with the OS separator
	}{
		{"simple", "file.txt", []string{"file.txt", objectFile}},
		{"nested", "path/to/file.txt", []string{"path", "to", "file.txt", objectFile}},
		{"prefix of another key", "foo/bar", []string{"foo", "bar", objectFile}},
		{"trailing delimiter", "asdf/", []string{"asdf", emptySegment, objectFile}},
		{"double delimiter", "a//b", []string{"a", emptySegment, "b", objectFile}},
		{"reserved leaf name", "#obj", []string{"##obj", objectFile}},
		{"reserved empty name", "#empty", []string{"##empty", objectFile}},
		{"already escaped", "##obj", []string{"###obj", objectFile}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			require.Equal(t, filepath.Join(tt.expected...), objectRelPath(tt.key))
		})
	}
}

// TestKeyFromContentPath covers the reverse mapping: every key must survive a
// round trip, and nothing but a content leaf may be read back as one.
func TestKeyFromContentPath(t *testing.T) {
	t.Parallel()

	keys := []string{
		"file.txt",
		"path/to/file.txt",
		"foo/bar",
		"foo/bar/baz",
		"asdf/",
		"a//b",
		"#obj",
		"#empty",
		"##obj",
		"weird #name/#obj",
	}

	for _, key := range keys {
		t.Run(key, func(t *testing.T) {
			t.Parallel()

			got, ok := keyFromContentPath(objectRelPath(key))
			require.True(t, ok)
			require.Equal(t, key, got)
		})
	}

	// A directory in the chain is not an object.
	_, ok := keyFromContentPath(filepath.Join("foo", "bar"))
	require.False(t, ok)

	// Neither is a stray file under some other name.
	_, ok = keyFromContentPath(filepath.Join("foo", "bar", "something-else"))
	require.False(t, ok)
}

// TestObjectRelPath_DistinctKeysDistinctPaths is the property the layout
// exists for: no two different keys may land on the same path, including the
// pairs that used to collide.
func TestObjectRelPath_DistinctKeysDistinctPaths(t *testing.T) {
	t.Parallel()

	keys := []string{
		"foo/bar", "foo/bar/baz", "foo", "asdf/", "asdf", "a//b", "a/b",
		"#obj", "##obj", "#empty", "foo/#obj",
	}

	seen := make(map[string]string, len(keys))

	for _, key := range keys {
		path := objectRelPath(key)
		if other, dup := seen[path]; dup {
			t.Fatalf("keys %q and %q both map to %q", other, key, path)
		}

		seen[path] = key
	}
}
