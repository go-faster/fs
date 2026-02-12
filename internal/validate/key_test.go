package validate

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestKey(t *testing.T) {
	tests := []struct {
		name      string
		key       string
		wantError bool
	}{
		// Valid keys - simple cases
		{
			name:      "simple filename",
			key:       "file.txt",
			wantError: false,
		},
		{
			name:      "with path",
			key:       "folder/file.txt",
			wantError: false,
		},
		{
			name:      "nested path",
			key:       "folder/subfolder/file.txt",
			wantError: false,
		},
		{
			name:      "deep nesting",
			key:       "a/b/c/d/e/f/file.txt",
			wantError: false,
		},
		{
			name:      "with spaces",
			key:       "my file.txt",
			wantError: false,
		},
		{
			name:      "with special chars",
			key:       "file-name_2023.txt",
			wantError: false,
		},
		{
			name:      "with dots in name",
			key:       "file.backup.txt",
			wantError: false,
		},
		{
			name:      "numbers only",
			key:       "12345",
			wantError: false,
		},

		// Valid keys - edge cases
		{
			name:      "single character",
			key:       "a",
			wantError: false,
		},
		{
			name:      "with leading slash",
			key:       "/file.txt",
			wantError: false,
		},
		{
			name:      "with leading slash and path",
			key:       "/folder/file.txt",
			wantError: false,
		},
		{
			name:      "trailing slash (directory marker)",
			key:       "folder/",
			wantError: false,
		},
		{
			name:      "directory marker nested",
			key:       "folder/subfolder/",
			wantError: false,
		},
		{
			name:      "with tab character",
			key:       "file\twith\ttabs.txt",
			wantError: false,
		},

		// Valid keys - special characters
		{
			name:      "with hyphen",
			key:       "my-file.txt",
			wantError: false,
		},
		{
			name:      "with underscore",
			key:       "my_file.txt",
			wantError: false,
		},
		{
			name:      "with parentheses",
			key:       "file(1).txt",
			wantError: false,
		},
		{
			name:      "with brackets",
			key:       "file[1].txt",
			wantError: false,
		},
		{
			name:      "with braces",
			key:       "file{1}.txt",
			wantError: false,
		},
		{
			name:      "with equals",
			key:       "key=value.txt",
			wantError: false,
		},
		{
			name:      "with plus",
			key:       "file+name.txt",
			wantError: false,
		},
		{
			name:      "with exclamation",
			key:       "file!important.txt",
			wantError: false,
		},
		{
			name:      "with ampersand",
			key:       "file&data.txt",
			wantError: false,
		},
		{
			name:      "with at symbol",
			key:       "user@host.txt",
			wantError: false,
		},
		{
			name:      "with hash",
			key:       "file#1.txt",
			wantError: false,
		},
		{
			name:      "with dollar",
			key:       "price$100.txt",
			wantError: false,
		},
		{
			name:      "with percent",
			key:       "50%off.txt",
			wantError: false,
		},
		{
			name:      "with comma",
			key:       "file,data.txt",
			wantError: false,
		},
		{
			name:      "with semicolon",
			key:       "file;data.txt",
			wantError: false,
		},
		{
			name:      "with colon",
			key:       "time:12:00.txt",
			wantError: false,
		},
		{
			name:      "with quotes",
			key:       "file'name.txt",
			wantError: false,
		},
		{
			name:      "with tilde",
			key:       "~backup.txt",
			wantError: false,
		},

		// Valid keys - Unicode
		{
			name:      "unicode characters",
			key:       "文件.txt",
			wantError: false,
		},
		{
			name:      "emoji in key",
			key:       "📁folder/📄file.txt",
			wantError: false,
		},
		{
			name:      "accented characters",
			key:       "café/résumé.txt",
			wantError: false,
		},
		{
			name:      "cyrillic characters",
			key:       "файл.txt",
			wantError: false,
		},
		{
			name:      "mixed unicode",
			key:       "folder/文件/file.txt",
			wantError: false,
		},

		// Invalid keys - path traversal (SECURITY)
		{
			name:      "parent directory",
			key:       "../file.txt",
			wantError: true,
		},
		{
			name:      "double parent",
			key:       "../../file.txt",
			wantError: true,
		},
		{
			name:      "parent in middle",
			key:       "folder/../file.txt",
			wantError: true,
		},
		{
			name:      "parent at end",
			key:       "folder/..",
			wantError: true,
		},
		{
			name:      "current directory",
			key:       "./file.txt",
			wantError: true,
		},
		{
			name:      "current in middle",
			key:       "folder/./file.txt",
			wantError: true,
		},
		{
			name:      "hidden parent",
			key:       "folder/..hidden/file.txt",
			wantError: true,
		},

		// Invalid keys - backslashes
		{
			name:      "backslash",
			key:       "folder\\file.txt",
			wantError: true,
		},
		{
			name:      "multiple backslashes",
			key:       "folder\\subfolder\\file.txt",
			wantError: true,
		},
		{
			name:      "windows path",
			key:       "C:\\Users\\file.txt",
			wantError: true,
		},

		// Invalid keys - absolute paths
		{
			name:      "unix absolute path",
			key:       "/etc/passwd",
			wantError: false, // Leading slash is allowed in S3
		},
		{
			name:      "windows absolute path",
			key:       "C:\\file.txt",
			wantError: true, // Has backslash
		},

		// Invalid keys - empty and null
		{
			name:      "empty key",
			key:       "",
			wantError: true,
		},
		{
			name:      "null byte",
			key:       "file\x00.txt",
			wantError: true,
		},
		{
			name:      "null byte at start",
			key:       "\x00file.txt",
			wantError: true,
		},
		{
			name:      "null byte at end",
			key:       "file.txt\x00",
			wantError: true,
		},

		// Invalid keys - control characters
		{
			name:      "newline character",
			key:       "file\nname.txt",
			wantError: true,
		},
		{
			name:      "carriage return",
			key:       "file\rname.txt",
			wantError: true,
		},
		{
			name:      "bell character",
			key:       "file\aname.txt",
			wantError: true,
		},
		{
			name:      "escape character",
			key:       "file\x1bname.txt",
			wantError: true,
		},
		{
			name:      "DEL character",
			key:       "file\x7fname.txt",
			wantError: true,
		},

		// Invalid keys - special cases
		{
			name:      "just slash",
			key:       "/",
			wantError: true,
		},
		{
			name:      "invalid UTF-8",
			key:       "file\xff\xfename.txt",
			wantError: true,
		},

		// Length validation
		{
			name:      "at max length",
			key:       strings.Repeat("a", 1024),
			wantError: false,
		},
		{
			name:      "over max length",
			key:       strings.Repeat("a", 1025),
			wantError: true,
		},
		{
			name:      "way over max length",
			key:       strings.Repeat("a", 2000),
			wantError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Key(tt.key)
			if tt.wantError {
				require.Error(t, err, "Key(%q) expected error, got nil", tt.key)
			} else {
				require.NoError(t, err, "Key(%q) unexpected error", tt.key)
			}
		})
	}
}

func TestKeySecurityFocused(t *testing.T) {
	t.Run("prevents path traversal", func(t *testing.T) {
		attacks := []string{
			"../etc/passwd",
			"../../etc/passwd",
			"folder/../../../etc/passwd",
			"./../../etc/passwd",
			"folder/../file.txt",
			"../file.txt",
			"..",
			"folder/..",
			"a/../b",
		}

		for _, attack := range attacks {
			err := Key(attack)
			require.Error(t, err, "should reject path traversal: %q", attack)
		}
	})

	t.Run("prevents backslash usage", func(t *testing.T) {
		attacks := []string{
			"folder\\file.txt",
			"C:\\Windows\\System32",
			"folder\\subfolder\\file.txt",
			"\\file.txt",
			"file\\name.txt",
		}

		for _, attack := range attacks {
			err := Key(attack)
			require.Error(t, err, "should reject backslashes: %q", attack)
		}
	})

	t.Run("prevents null byte injection", func(t *testing.T) {
		attacks := []string{
			"file\x00.txt",
			"\x00file.txt",
			"file.txt\x00",
			"folder/\x00/file.txt",
		}

		for _, attack := range attacks {
			err := Key(attack)
			require.Error(t, err, "should reject null bytes: %q", attack)
		}
	})

	t.Run("prevents control character injection", func(t *testing.T) {
		attacks := []string{
			"file\nname.txt",
			"file\rname.txt",
			"file\aname.txt",
			"file\bname.txt",
			"file\fname.txt",
			"file\vname.txt",
			"file\x1bname.txt",
			"file\x7fname.txt", // DEL
		}

		for _, attack := range attacks {
			err := Key(attack)
			require.Error(t, err, "should reject control characters: %q", attack)
		}
	})

	t.Run("prevents relative path references", func(t *testing.T) {
		attacks := []string{
			"./file.txt",
			"./folder/file.txt",
			"folder/./file.txt",
			"../folder/file.txt",
		}

		for _, attack := range attacks {
			err := Key(attack)
			require.Error(t, err, "should reject relative path references: %q", attack)
		}
	})
}

func TestKeyAWSCompliance(t *testing.T) {
	t.Run("allows valid AWS S3 keys", func(t *testing.T) {
		validKeys := []string{
			"file.txt",
			"folder/file.txt",
			"my-organization/my-project/file.txt",
			"logs/2023/01/15/app.log",
			"images/photo (1).jpg",
			"documents/résumé.pdf",
			"data/file_v2.0.txt",
			"/leading/slash/file.txt",
			"trailing/slash/",
			"file with spaces.txt",
			"special!chars@file#1.txt",
		}

		for _, key := range validKeys {
			err := Key(key)
			require.NoError(t, err, "valid AWS S3 key should be accepted: %q", key)
		}
	})

	t.Run("common S3 key patterns", func(t *testing.T) {
		patterns := []string{
			"images/2023/01/photo.jpg",
			"logs/application/2023-01-15.log",
			"backups/database-2023-01-15-10-30-00.sql",
			"uploads/user123/avatar.png",
			"documents/invoices/2023/INV-001.pdf",
			"data/exports/customers-20230115.csv",
		}

		for _, pattern := range patterns {
			err := Key(pattern)
			require.NoError(t, err, "common S3 pattern should be accepted: %q", pattern)
		}
	})

	t.Run("length boundaries", func(t *testing.T) {
		// Test at boundary
		require.NoError(t, Key(strings.Repeat("a", 1024)), "1024 bytes should be at max")
		require.Error(t, Key(strings.Repeat("a", 1025)), "1025 bytes should exceed max")

		// Test with multi-byte UTF-8
		// Each emoji is 4 bytes
		emoji := "😀"
		require.Equal(t, 4, len(emoji), "emoji should be 4 bytes")

		// 256 emojis = 1024 bytes (exactly at limit)
		maxEmoji := strings.Repeat(emoji, 256)
		require.Equal(t, 1024, len(maxEmoji))
		require.NoError(t, Key(maxEmoji), "1024 bytes of emoji should be valid")

		// 257 emojis = 1028 bytes (over limit)
		overEmoji := strings.Repeat(emoji, 257)
		require.Equal(t, 1028, len(overEmoji))
		require.Error(t, Key(overEmoji), "1028 bytes of emoji should be invalid")
	})
}

func TestKeyUTF8Validation(t *testing.T) {
	t.Run("valid UTF-8", func(t *testing.T) {
		validUTF8 := []string{
			"hello",
			"世界",
			"🌍🌎🌏",
			"café",
			"Москва",
			"こんにちは",
		}

		for _, key := range validUTF8 {
			err := Key(key)
			require.NoError(t, err, "valid UTF-8 should be accepted: %q", key)
		}
	})

	t.Run("invalid UTF-8", func(t *testing.T) {
		invalidUTF8 := []string{
			"hello\xff\xfe",
			"world\xc0\xc1",
			"\xf5\xf6\xf7",
		}

		for _, key := range invalidUTF8 {
			err := Key(key)
			require.Error(t, err, "invalid UTF-8 should be rejected: %q", key)
		}
	})
}

func TestKeyEdgeCases(t *testing.T) {
	t.Run("directory markers", func(t *testing.T) {
		// S3 allows keys ending with / to represent directories
		require.NoError(t, Key("folder/"))
		require.NoError(t, Key("folder/subfolder/"))
		require.NoError(t, Key("a/b/c/d/"))
	})

	t.Run("leading slash behavior", func(t *testing.T) {
		// Leading slash is allowed but not recommended
		require.NoError(t, Key("/file.txt"))
		require.NoError(t, Key("/folder/file.txt"))

		// Just slash is not allowed
		require.Error(t, Key("/"))
	})

	t.Run("dots in filenames", func(t *testing.T) {
		// Single dots in names are fine
		require.NoError(t, Key("file.txt"))
		require.NoError(t, Key("file.backup.txt"))
		require.NoError(t, Key("folder/.hidden"))

		// But .. is not allowed (path traversal)
		require.Error(t, Key("folder/../file.txt"))
		require.Error(t, Key(".."))
	})

	t.Run("special filenames", func(t *testing.T) {
		require.NoError(t, Key(".gitignore"))
		require.NoError(t, Key(".htaccess"))
		require.NoError(t, Key("folder/.hidden-file"))
		require.NoError(t, Key("~backup-file"))
	})
}

func BenchmarkKey(b *testing.B) {
	testCases := []struct {
		name string
		key  string
	}{
		{"simple", "file.txt"},
		{"nested", "folder/subfolder/file.txt"},
		{"with spaces", "my file name.txt"},
		{"unicode", "文件/📁/file.txt"},
		{"invalid traversal", "../etc/passwd"},
		{"long key", strings.Repeat("a/", 100) + "file.txt"},
	}

	for _, tc := range testCases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()

			for i := 0; i < b.N; i++ {
				_ = Key(tc.key)
			}
		})
	}
}
