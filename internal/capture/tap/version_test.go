package tap

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParseOSVersion(t *testing.T) {
	t.Parallel()
	tests := map[string]struct {
		ok       bool
		want     osVersion
		supports bool
	}{
		"15.8.2":    {true, osVersion{15, 8, 2}, true},
		"14.2":      {true, osVersion{14, 2, 0}, true},
		"14.1.2":    {true, osVersion{14, 1, 2}, false},
		"13.6.9":    {true, osVersion{13, 6, 9}, false},
		"26":        {true, osVersion{26, 0, 0}, true},
		" 14.10 \n": {true, osVersion{14, 10, 0}, true},
		"":          {false, osVersion{}, false},
		"14.x":      {false, osVersion{}, false},
		"1.2.3.4":   {false, osVersion{}, false},
		"-1.0":      {false, osVersion{}, false},
	}
	for in, tc := range tests {
		got, ok := parseOSVersion(in)
		assert.Equal(t, tc.ok, ok, "%q", in)
		assert.Equal(t, tc.want, got, "%q", in)
		assert.Equal(t, tc.supports, got.supportsTaps(), "%q", in)
	}
	assert.Equal(t, "14.2.0", osVersion{14, 2, 0}.String())
}
