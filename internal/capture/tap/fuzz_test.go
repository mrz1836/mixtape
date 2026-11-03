package tap

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// FuzzParseOSVersion checks parseOSVersion against its documented
// contract on any text: up to three dot-separated integers, none negative,
// missing parts zero, and false with a zero version for anything else.
func FuzzParseOSVersion(f *testing.F) {
	for _, s := range []string{
		"15.8.2", "14.2", "14.1.2", "13.6.9", "26", " 14.10 \n",
		"", "14.x", "1.2.3.4", "-1.0", "+14.2", "14..2", "99999999999999999999",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, s string) {
		v, ok := parseOSVersion(s)
		if !ok {
			assert.Equal(t, osVersion{}, v, "a rejected version is zero")
			return
		}
		assert.GreaterOrEqual(t, v.major, 0)
		assert.GreaterOrEqual(t, v.minor, 0)
		assert.GreaterOrEqual(t, v.patch, 0)

		parts := strings.Split(strings.TrimSpace(s), ".")
		require.LessOrEqual(t, len(parts), 3, "%q has too many parts", s)
		for _, p := range parts {
			digits := strings.TrimLeft(p, "+-")
			require.LessOrEqual(t, len(p)-len(digits), 1, "%q: more than one sign", s)
			require.NotEmpty(t, digits, "%q: a part without digits", s)
			require.Empty(t, strings.Trim(digits, "0123456789"), "%q: a part that is not an integer", s)
		}
		if len(parts) < 3 {
			assert.Zero(t, v.patch, "%q: a missing patch is zero", s)
		}
		if len(parts) < 2 {
			assert.Zero(t, v.minor, "%q: a missing minor is zero", s)
		}

		back, ok := parseOSVersion(v.String())
		require.True(t, ok, "%q does not parse back", v.String())
		assert.Equal(t, v, back)
	})
}
