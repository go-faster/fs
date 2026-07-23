package clusterstore

import (
	"bytes"
	"io"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/time/rate"
)

func TestThrottledReaderCapsReadsAtBurst(t *testing.T) {
	payload := randBytes(4096)

	r := &throttledReader{
		rc:      io.NopCloser(bytes.NewReader(payload)),
		ctx:     t.Context(),
		limiter: rate.NewLimiter(1<<30, 64),
	}

	buf := make([]byte, 1024)

	n, err := r.Read(buf)
	require.NoError(t, err)
	assert.Equal(t, 64, n, "a read never exceeds the limiter burst")

	got, err := io.ReadAll(r)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(payload, append(buf[:n], got...)), "throttling must not corrupt the stream")
	require.NoError(t, r.Close())
}
