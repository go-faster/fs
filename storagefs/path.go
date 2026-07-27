package storagefs

import (
	"path/filepath"
	"strings"
)

// S3 keys are flat: "foo/bar" and "foo/bar/baz" are two unrelated names that
// must be able to coexist, and "asdf/" is a perfectly ordinary key. A
// filesystem path is not flat, so mapping a key straight onto one leaves those
// cases unrepresentable — the second PUT finds a file where it needs a
// directory and fails, which is how a legal pair of keys used to become an
// InternalError and a lost write.
//
// So a key does not name a file here; it names a *directory*, and the content
// lives in a reserved leaf inside it:
//
//	foo/bar      -> foo/bar/#obj
//	foo/bar/baz  -> foo/bar/baz/#obj
//	asdf/        -> asdf/#empty/#obj
//
// No key's shape can then collide with another's. The cost is one directory
// per key component, which is what filesystems are built for.
const (
	// objectFile holds an object's content inside its key directory.
	objectFile = "#obj"
	// emptySegment stands in for an empty key component, which cannot be a
	// directory name. Keys containing "//" or ending in "/" produce one.
	emptySegment = "#empty"
	// escapePrefix distinguishes a real key component that happens to look
	// like one of the reserved names above from the reserved name itself.
	escapePrefix = "#"
)

// objectRelPath returns the path of key's content file, relative to its bucket
// directory, in native OS separators.
func objectRelPath(key string) string {
	parts := strings.Split(key, "/")
	segments := make([]string, 0, len(parts)+1)

	for _, part := range parts {
		segments = append(segments, encodeSegment(part))
	}

	segments = append(segments, objectFile)

	return filepath.Join(segments...)
}

// keyFromContentPath reverses objectRelPath: given a path relative to the
// bucket directory, it reports the key whose content lives there, and whether
// the path names an object leaf at all.
func keyFromContentPath(rel string) (string, bool) {
	segments := strings.Split(filepath.ToSlash(rel), "/")
	if len(segments) < 2 || segments[len(segments)-1] != objectFile {
		return "", false
	}

	parts := make([]string, 0, len(segments)-1)
	for _, seg := range segments[:len(segments)-1] {
		parts = append(parts, decodeSegment(seg))
	}

	return strings.Join(parts, "/"), true
}

// encodeSegment renders one key component as a directory name.
func encodeSegment(part string) string {
	if part == "" {
		return emptySegment
	}

	// A component that already starts with the escape prefix gets one more, so
	// it can never be read back as a reserved name.
	if strings.HasPrefix(part, escapePrefix) {
		return escapePrefix + part
	}

	return part
}

// decodeSegment reverses encodeSegment.
func decodeSegment(seg string) string {
	if seg == emptySegment {
		return ""
	}

	return strings.TrimPrefix(seg, escapePrefix)
}
