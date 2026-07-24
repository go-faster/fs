package auth_test

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/auth"
)

func TestSealerRoundTrip(t *testing.T) {
	s, err := auth.NewSealer([]byte("cluster-secret-0123456789"))
	require.NoError(t, err)

	secret := "exampleSecretKey/40charsAAAAAAAAAAAAAAAA"

	blob, err := s.Seal(secret)
	require.NoError(t, err)
	assert.NotContains(t, blob, secret, "sealed blob must not contain the plaintext")

	got, err := s.Open(blob)
	require.NoError(t, err)
	assert.Equal(t, secret, got)
}

func TestSealerNonceIsRandom(t *testing.T) {
	s, err := auth.NewSealer([]byte("cluster-secret-0123456789"))
	require.NoError(t, err)

	a, err := s.Seal("same")
	require.NoError(t, err)

	b, err := s.Seal("same")
	require.NoError(t, err)

	assert.NotEqual(t, a, b, "sealing the same secret twice must differ (fresh nonce)")

	for _, blob := range []string{a, b} {
		got, err := s.Open(blob)
		require.NoError(t, err)
		assert.Equal(t, "same", got)
	}
}

func TestSealerWrongSecretFails(t *testing.T) {
	sealer, err := auth.NewSealer([]byte("cluster-secret-0123456789"))
	require.NoError(t, err)

	blob, err := sealer.Seal("top-secret")
	require.NoError(t, err)

	other, err := auth.NewSealer([]byte("a-different-cluster-secret"))
	require.NoError(t, err)

	_, err = other.Open(blob)
	require.Error(t, err, "opening with a different cluster secret must fail")
}

func TestSealerTamperFails(t *testing.T) {
	s, err := auth.NewSealer([]byte("cluster-secret-0123456789"))
	require.NoError(t, err)

	blob, err := s.Seal("top-secret")
	require.NoError(t, err)

	// Flip the last base64 character to a different one; GCM authentication
	// must reject the corrupted ciphertext.
	last := blob[len(blob)-1]

	repl := byte('A')
	if last == 'A' {
		repl = 'B'
	}

	tampered := blob[:len(blob)-1] + string(repl)

	_, err = s.Open(tampered)
	require.Error(t, err)
}

func TestSealerRejectsMalformed(t *testing.T) {
	s, err := auth.NewSealer([]byte("cluster-secret-0123456789"))
	require.NoError(t, err)

	_, err = s.Open("not base64 $$$")
	require.Error(t, err)

	_, err = s.Open(strings.Repeat("A", 4)) // Valid base64, too short for a nonce.
	require.Error(t, err)
}

func TestNewSealerRejectsEmptySecret(t *testing.T) {
	_, err := auth.NewSealer(nil)
	require.Error(t, err)
}

func TestPermissionStringRoundTrip(t *testing.T) {
	for _, p := range []auth.Permission{auth.Read, auth.Write, auth.Admin} {
		got, err := auth.ParsePermission(p.String())
		require.NoError(t, err)
		assert.Equal(t, p, got)
	}

	_, err := auth.ParsePermission("root")
	require.Error(t, err)
}
