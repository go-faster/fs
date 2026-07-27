package checksum_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/checksum"
)

// The digests ceph/s3-tests asserts, over the bodies it uses. They are the
// point of this test: they were computed by someone else, so agreeing with
// them means the algorithm is right rather than merely self-consistent.
func TestMatchesSuiteVectors(t *testing.T) {
	oneKiBofA := strings.Repeat("A", 1024)

	for _, tc := range []struct {
		algorithm checksum.Algorithm
		body      string
		want      string
	}{
		{checksum.SHA256, oneKiBofA, "arcu6553sHVAiX4MjW0j7I7vD4w6R+Gz9Ok0Q9lTa+0="},
		{checksum.CRC64NVME, oneKiBofA, "Qeh8oXvGiSo="},

		// The 5 MiB parts the multipart tests upload.
		{checksum.SHA256, strings.Repeat("A", 5*1024*1024), "275VF5loJr1YYawit0XSHREhkFXYkkPKGuoK0x9VKxI="},
		{checksum.SHA256, strings.Repeat("B", 5*1024*1024), "mrHwOfjTL5Zwfj74F05HOQGLdUb7E5szdCbxgUSq6NM="},
		{checksum.SHA1, strings.Repeat("A", 5*1024*1024), "iIaTCGbm+vdVjNqIMF2S0T7ibMk="},
		{checksum.CRC32, strings.Repeat("A", 5*1024*1024), "JRTCyQ=="},
		{checksum.CRC32C, strings.Repeat("A", 5*1024*1024), "MDaLrw=="},
		{checksum.CRC64NVME, strings.Repeat("A", 5*1024*1024), "L/E4WYn8v98="},
	} {
		t.Run(string(tc.algorithm)+"/"+string(tc.body[0]), func(t *testing.T) {
			h, err := tc.algorithm.New()
			require.NoError(t, err)

			_, err = h.Write([]byte(tc.body))
			require.NoError(t, err)

			require.Equal(t, tc.want, checksum.Encode(h.Sum(nil)))
		})
	}
}

// TestCompositeMatchesSuiteVectors pins the composition rule, which is where a
// plausible-but-wrong implementation hides: hashing the parts' bytes again
// instead of their digests produces a valid-looking value that is not S3's.
func TestCompositeMatchesSuiteVectors(t *testing.T) {
	for _, tc := range []struct {
		algorithm checksum.Algorithm
		parts     []string
		want      string
	}{
		{
			checksum.SHA256,
			[]string{
				"275VF5loJr1YYawit0XSHREhkFXYkkPKGuoK0x9VKxI=",
				"mrHwOfjTL5Zwfj74F05HOQGLdUb7E5szdCbxgUSq6NM=",
				"Vw7oB/nKQ5xWb3hNgbyfkvDiivl+U+/Dft48nfJfDow=",
			},
			"uWBwpe1dxI4Vw8Gf0X9ynOdw/SS6VBzfWm9giiv1sf4=-3",
		},
		{
			checksum.SHA1,
			[]string{
				"iIaTCGbm+vdVjNqIMF2S0T7ibMk=",
				"LS/TJ32bAVKEwRu+sE3X7awh/lk=",
				"6DDwovUaHwrKNXDMzOGbuvj9kxI=",
			},
			"sizjvY4eud3MrcHdZM3cQ/ol39o=-3",
		},
	} {
		t.Run(string(tc.algorithm), func(t *testing.T) {
			digests := make([][]byte, 0, len(tc.parts))

			for _, p := range tc.parts {
				raw, err := tc.algorithm.Decode(p)
				require.NoError(t, err)

				digests = append(digests, raw)
			}

			got, err := checksum.CompositeOf(tc.algorithm, digests)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

// TestFullObjectOnlyForCRCs: only the CRCs compose into a whole-object digest.
// Offering FULL_OBJECT for SHA would mean claiming a value that cannot be
// computed from parts without re-reading the object.
func TestFullObjectOnlyForCRCs(t *testing.T) {
	for a, want := range map[checksum.Algorithm]bool{
		checksum.CRC32: true, checksum.CRC32C: true, checksum.CRC64NVME: true,
		checksum.SHA1: false, checksum.SHA256: false,
	} {
		require.Equal(t, want, a.SupportsFullObject(), string(a))

		if want {
			require.Equal(t, checksum.FullObject, a.DefaultType(), string(a))
		} else {
			require.Equal(t, checksum.Composite, a.DefaultType(), string(a))
		}
	}
}

func TestSplitComposite(t *testing.T) {
	digest, parts, ok := checksum.SplitComposite("uWBwpe1dxI4Vw8Gf0X9ynOdw/SS6VBzfWm9giiv1sf4=-3")
	require.True(t, ok)
	require.Equal(t, 3, parts)
	require.Equal(t, "uWBwpe1dxI4Vw8Gf0X9ynOdw/SS6VBzfWm9giiv1sf4=", digest)

	// A full-object value has no suffix, and base64 padding must not be read
	// as one.
	_, _, ok = checksum.SplitComposite("xU+Krw==")
	require.False(t, ok)
}

// TestDecodeRejectsWrongLength keeps "you sent nonsense" separate from "your
// bytes did not match".
func TestDecodeRejectsWrongLength(t *testing.T) {
	_, err := checksum.SHA256.Decode("Qeh8oXvGiSo=")
	require.Error(t, err, "an 8-byte digest is not a SHA256")

	_, err = checksum.SHA256.Decode("bad")
	require.Error(t, err)

	_, err = checksum.SHA256.Decode("arcu6553sHVAiX4MjW0j7I7vD4w6R+Gz9Ok0Q9lTa+0=")
	require.NoError(t, err)
}

func TestParseAndHeader(t *testing.T) {
	a, err := checksum.Parse("sha256")
	require.NoError(t, err)
	require.Equal(t, checksum.SHA256, a)
	require.Equal(t, "x-amz-checksum-sha256", a.Header())

	a, err = checksum.Parse("")
	require.NoError(t, err)
	require.Empty(t, a)
	require.Empty(t, a.Header())

	_, err = checksum.Parse("md5")
	require.Error(t, err)
}
