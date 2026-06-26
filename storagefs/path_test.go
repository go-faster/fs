package storagefs

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestToOSPath(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{
			name:     "simple filename",
			input:    "file.txt",
			expected: "file.txt",
		},
		{
			name:     "nested path",
			input:    "path/to/file.txt",
			expected: filepath.Join("path", "to", "file.txt"),
		},
		{
			name:     "deep nested path",
			input:    "a/b/c/d/e/file.txt",
			expected: filepath.Join("a", "b", "c", "d", "e", "file.txt"),
		},
		{
			name:     "single directory",
			input:    "directory/",
			expected: "directory" + string(filepath.Separator),
		},
		{
			name:     "multiple slashes",
			input:    "path//to///file.txt",
			expected: "path" + string(filepath.Separator) + string(filepath.Separator) + "to" + string(filepath.Separator) + string(filepath.Separator) + string(filepath.Separator) + "file.txt",
		},
		{
			name:     "empty string",
			input:    "",
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := toOSPath(tt.input)
			require.Equal(t, tt.expected, result)
		})
	}
}
