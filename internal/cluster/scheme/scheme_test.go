package scheme_test

import (
	"fmt"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/go-faster/fs/internal/cluster/scheme"
)

func TestParseFormatRoundTrip(t *testing.T) {
	cases := []struct {
		in   string
		want scheme.Scheme
		str  string
	}{
		{"rf2.5", scheme.Scheme{Kind: scheme.RF25}, "rf2.5"},
		{"RF2.5", scheme.Scheme{Kind: scheme.RF25}, "rf2.5"},
		{"rf3", scheme.Scheme{Kind: scheme.RF3}, "rf3"},
		{"ec:4,2", scheme.Scheme{Kind: scheme.EC, K: 4, M: 2}, "ec:4,2"},
		{"ec:10,4", scheme.Scheme{Kind: scheme.EC, K: 10, M: 4}, "ec:10,4"},
		{" ec:4, 2 ", scheme.Scheme{Kind: scheme.EC, K: 4, M: 2}, "ec:4,2"},
	}

	for _, c := range cases {
		got, err := scheme.Parse(c.in)
		require.NoError(t, err, c.in)
		assert.Equal(t, c.want, got, c.in)
		assert.Equal(t, c.str, got.String(), c.in)

		// String output must itself re-parse to the same scheme.
		again, err := scheme.Parse(got.String())
		require.NoError(t, err)
		assert.Equal(t, got, again)
	}
}

func TestParseInvalid(t *testing.T) {
	for _, in := range []string{"", "rf", "rf2", "rf4", "ec", "ec:", "ec:4", "ec:4,", "ec:a,2", "ec:4,x", "ec:0,2", "ec:4,0", "raid5"} {
		_, err := scheme.Parse(in)
		assert.Error(t, err, "%q should be invalid", in)
	}
}

func TestSchemeProperties(t *testing.T) {
	rf25 := scheme.Scheme{Kind: scheme.RF25}
	assert.Equal(t, 3, rf25.Copies())
	assert.Equal(t, 2, rf25.FullReplicas())
	assert.Equal(t, 2, rf25.WriteQuorum())
	assert.Equal(t, 1, rf25.Tolerance())
	assert.InDelta(t, 2.5, rf25.Overhead(), 1e-9)

	rf3 := scheme.Scheme{Kind: scheme.RF3}
	assert.Equal(t, 3, rf3.Copies())
	assert.Equal(t, 3, rf3.FullReplicas())
	assert.Equal(t, 2, rf3.WriteQuorum())
	assert.Equal(t, 2, rf3.Tolerance())
	assert.InDelta(t, 3.0, rf3.Overhead(), 1e-9)

	ec := scheme.Scheme{Kind: scheme.EC, K: 4, M: 2}
	assert.Equal(t, 6, ec.Copies())
	assert.Equal(t, 0, ec.FullReplicas())
	assert.Equal(t, 6, ec.WriteQuorum())
	assert.Equal(t, 2, ec.Tolerance())
	assert.InDelta(t, 1.5, ec.Overhead(), 1e-9)
}

func TestDefaults(t *testing.T) {
	require.NoError(t, scheme.Default.Validate())
	require.NoError(t, scheme.DefaultEC.Validate())
	assert.Equal(t, scheme.RF25, scheme.Default.Kind)
	assert.Equal(t, "ec:4,2", scheme.DefaultEC.String())
	assert.InDelta(t, 1.5, scheme.DefaultEC.Overhead(), 1e-9)
}

func TestValidate(t *testing.T) {
	assert.Error(t, scheme.Scheme{Kind: scheme.RF25, K: 1}.Validate())
	assert.Error(t, scheme.Scheme{Kind: scheme.EC, K: 0, M: 2}.Validate())
	assert.Error(t, scheme.Scheme{Kind: scheme.EC, K: 4, M: 0}.Validate())
	assert.NoError(t, scheme.Scheme{Kind: scheme.EC, K: 1, M: 1}.Validate())
}

// randBytes returns n deterministic pseudo-random bytes (seeded, so failures
// reproduce).
func randBytes(n int) []byte {
	r := rand.New(rand.NewSource(int64(n)*2654435761 + 1)) //nolint:gosec // Deterministic test data, not crypto.
	b := make([]byte, n)
	_, _ = r.Read(b)

	return b
}

func ExampleScheme_String() {
	fmt.Println(scheme.Scheme{Kind: scheme.EC, K: 4, M: 2})
	// Output: ec:4,2
}
