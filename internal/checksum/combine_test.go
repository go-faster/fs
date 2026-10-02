package checksum_test

import (
	"math/rand/v2"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/checksum"
)

func sum(t *testing.T, a checksum.Algorithm, data []byte) []byte {
	t.Helper()

	h, err := a.New()
	require.NoError(t, err)

	_, _ = h.Write(data)

	return h.Sum(nil)
}

func TestCombine(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))

	for _, a := range []checksum.Algorithm{checksum.CRC32, checksum.CRC32C, checksum.CRC64NVME} {
		for range 200 {
			data := make([]byte, r.IntN(5000))
			for i := range data {
				data[i] = byte(r.Uint32())
			}

			// Fold random parts, the way a multipart upload's are combined.
			var (
				acc   []byte
				start int
			)

			for start < len(data) || acc == nil {
				end := min(len(data), start+r.IntN(1200))
				part := data[start:end]

				if acc == nil {
					acc = sum(t, a, part)
				} else {
					var err error

					acc, err = a.Combine(acc, sum(t, a, part), int64(len(part)))
					require.NoError(t, err)
				}

				start = end
				if start == len(data) {
					break
				}
			}

			require.Equal(t, sum(t, a, data), acc, "%s over %d bytes", a, len(data))
		}
	}
}

func TestCombineRefusesSHA(t *testing.T) {
	_, err := checksum.SHA256.Combine(make([]byte, 32), make([]byte, 32), 1)
	require.Error(t, err)
}
