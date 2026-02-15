package storagefs

import (
	"testing"
)

// TestToOSPath_BrokenImplementationWouldFail demonstrates what would happen
// if toOSPath was implemented incorrectly (just returning the key as-is).
func TestToOSPath_BrokenImplementationWouldFail(t *testing.T) {
	t.Parallel()

	// Correct implementation.
	correctResult := toOSPath("path/to/file.txt")

	// Broken implementation (just returns key as-is).
	brokenToOSPath := func(key string) string {
		return key // BROKEN: doesn't convert slashes
	}
	brokenResult := brokenToOSPath("path/to/file.txt")

	// On Windows, these would be different.
	// On Unix, they would be the same.
	// The test in windows_validation_test.go checks the actual file system paths,
	// which would fail on Windows with the broken implementation.

	t.Logf("Correct toOSPath result: %q", correctResult)
	t.Logf("Broken toOSPath result: %q", brokenResult)

	// This demonstrates the issue - on Windows:
	// - Correct: "path\\to\\file.txt"
	// - Broken: "path/to/file.txt"
	//
	// The broken version would cause:
	// - os.Create to create a file named "path/to/file.txt" (literal slashes in filename)
	// - Directory structure would not be created
	// - GetObject would fail because it looks for "path\to\file.txt"
}

// TestToOSPath_AlternativeBrokenImplementation demonstrates another common mistake.
func TestToOSPath_AlternativeBrokenImplementation(t *testing.T) {
	t.Parallel()

	// Another broken implementation - always converts to backslashes.
	alwaysBackslashToOSPath := func(key string) string {
		return key // BROKEN on Unix: doesn't check filepath.Separator
	}

	// This would break on Unix systems.
	result := alwaysBackslashToOSPath("path/to/file.txt")
	t.Logf("Always-backslash result: %q", result)

	// Correct implementation adapts to the OS.
	correctResult := toOSPath("path/to/file.txt")
	t.Logf("Correct result: %q", correctResult)
}

// TestToOSPath_PartialImplementation demonstrates partial conversion mistake.
func TestToOSPath_PartialImplementation(t *testing.T) {
	t.Parallel()

	// Broken: only converts first slash.
	partialToOSPath := func(key string) string {
		// BROKEN: only replaces first occurrence
		if len(key) > 0 && key[0] == '/' {
			return "\\" + key[1:]
		}
		return key
	}

	input := "path/to/file.txt"
	brokenResult := partialToOSPath(input)
	correctResult := toOSPath(input)

	t.Logf("Input: %q", input)
	t.Logf("Partial (broken) conversion: %q", brokenResult)
	t.Logf("Correct conversion: %q", correctResult)

	// The partial implementation would fail because:
	// Input: "path/to/file.txt"
	// Broken result: "path/to/file.txt" (no change since it doesn't start with /)
	// Correct result on Windows: "path\\to\\file.txt"
}
