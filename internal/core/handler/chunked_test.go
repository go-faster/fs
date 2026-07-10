package handler

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAWSChunkedReader(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			name: "single signed chunk",
			body: "d;chunk-signature=0000000000000000000000000000000000000000000000000000000000000000\r\nhello, world!\r\n0;chunk-signature=1111111111111111111111111111111111111111111111111111111111111111\r\n\r\n",
			want: "hello, world!",
		},
		{
			name: "multiple chunks",
			body: "5;chunk-signature=aaaa\r\nhello\r\n5;chunk-signature=bbbb\r\nworld\r\n0;chunk-signature=cccc\r\n\r\n",
			want: "helloworld",
		},
		{
			name: "unsigned trailer variant",
			body: "b\r\nhello world\r\n0\r\nx-amz-checksum-crc32c:abcd1234\r\n\r\n",
			want: "hello world",
		},
		{
			name: "empty payload",
			body: "0;chunk-signature=aaaa\r\n\r\n",
			want: "",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := io.ReadAll(newAWSChunkedReader(strings.NewReader(c.body)))
			require.NoError(t, err)
			require.Equal(t, c.want, string(got))
		})
	}
}

// TestAWSChunkedReader_SmallBuffer verifies decoding is correct when the
// consumer reads in tiny increments that straddle chunk boundaries.
func TestAWSChunkedReader_SmallBuffer(t *testing.T) {
	body := "5;chunk-signature=aaaa\r\nhello\r\n6;chunk-signature=bbbb\r\n world\r\n0\r\n\r\n"
	r := newAWSChunkedReader(strings.NewReader(body))

	var out []byte

	buf := make([]byte, 1)

	for {
		n, err := r.Read(buf)
		out = append(out, buf[:n]...)

		if err == io.EOF {
			break
		}

		require.NoError(t, err)
	}

	require.Equal(t, "hello world", string(out))
}
