package bench

import (
	"fmt"
	"strconv"
	"testing"
)

// sizeName renders a byte size for benchmark sub-names.
func sizeName(n int64) string {
	switch {
	case n >= 1<<20:
		return strconv.FormatInt(n>>20, 10) + "MiB"
	case n >= 1<<10:
		return strconv.FormatInt(n>>10, 10) + "KiB"
	default:
		return strconv.FormatInt(n, 10) + "B"
	}
}

// BenchmarkPutObject measures write throughput (b.SetBytes → MB/s) and, with
// -benchmem, allocations per PUT across object sizes.
func BenchmarkPutObject(b *testing.B) {
	for _, size := range []int64{sizeSmall, sizeMid, sizeLarge} {
		b.Run(sizeName(size), func(b *testing.B) {
			s, _ := benchStore(b)
			body := newBody(size)

			b.SetBytes(size)
			b.ReportAllocs()
			b.ResetTimer()

			for i := 0; b.Loop(); i++ {
				putObject(b, s, fmt.Sprintf("put-%d", i), size, body)
			}
		})
	}
}

// BenchmarkGetObject measures read throughput across object sizes.
func BenchmarkGetObject(b *testing.B) {
	for _, size := range []int64{sizeSmall, sizeMid, sizeLarge} {
		b.Run(sizeName(size), func(b *testing.B) {
			s, _ := benchStore(b)

			const key = "get"

			putObject(b, s, key, size, newBody(size))

			b.SetBytes(size)
			b.ReportAllocs()
			b.ResetTimer()

			for b.Loop() {
				getObjectDiscard(b, s, key)
			}
		})
	}
}

// BenchmarkPutGetRoundTrip measures a write-then-read cycle for small objects
// — the metadata-bound path where per-request overhead dominates.
func BenchmarkPutGetRoundTrip(b *testing.B) {
	s, _ := benchStore(b)
	body := newBody(sizeSmall)

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; b.Loop(); i++ {
		key := fmt.Sprintf("rt-%d", i)
		putObject(b, s, key, sizeSmall, body)
		getObjectDiscard(b, s, key)
	}
}
