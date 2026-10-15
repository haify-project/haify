package drbdmodule

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in                  string
		major, minor, patch int
	}{
		{"9.3.4", 9, 3, 4},
		{"v9.3.4", 9, 3, 4},
		{"9.2", 9, 2, 0},
		{"9.2.0-rc.7", 9, 2, 0},
		{" 8.4.11\n", 8, 4, 11},
		{"9.1.23+ptf.1", 9, 1, 23},
	}
	for _, c := range cases {
		v, err := ParseVersion(c.in)
		require.NoError(t, err, c.in)
		assert.Equal(t, [3]int{c.major, c.minor, c.patch}, [3]int{v.Major, v.Minor, v.Patch}, c.in)
	}
	for _, bad := range []string{"", "9", "nine.two", "9.x.1", "1.2.3.4", "-1.2"} {
		_, err := ParseVersion(bad)
		assert.Error(t, err, bad)
	}
}

func TestVersionOrdering(t *testing.T) {
	v := func(s string) Version {
		x, err := ParseVersion(s)
		require.NoError(t, err)
		return x
	}
	assert.True(t, v("8.4.11").Less(v("9.2.0")))
	assert.True(t, v("9.1.23").Less(v("9.2.0")))
	assert.True(t, v("9.2.0").Less(v("9.2.1")))
	assert.False(t, v("9.3.4").Less(v("9.2.0")))
	assert.True(t, v("9.2.0").AtLeast(v("9.2")))
	assert.True(t, v("10.0.0").AtLeast(v("9.9.9")))
	// The numeric part orders, so 9.2.0-rc.7 satisfies a 9.2.0 minimum.
	assert.True(t, v("9.2.0-rc.7").AtLeast(v("9.2.0")))
	assert.Equal(t, "9.2.0-rc.7", v("9.2.0-rc.7").String())
	assert.Equal(t, "9.2.0", Version{Major: 9, Minor: 2}.String())
}

func TestParseProcDRBD(t *testing.T) {
	v, err := ParseProcDRBD("version: 9.3.4 (api:2/proto:86-124)\nGIT-hash: abc build by root@x, 2026-01-01\nTransports (api:21): tcp (9.3.4)\n")
	require.NoError(t, err)
	assert.Equal(t, "9.3.4", v.String())

	v, err = ParseProcDRBD("version: 8.4.11 (api:1/proto:86-101)\nsrcversion: 1234\n")
	require.NoError(t, err)
	assert.Equal(t, 8, v.Major)

	_, err = ParseProcDRBD("GIT-hash: abc\n")
	assert.Error(t, err)
	_, err = ParseProcDRBD("version:\n")
	assert.Error(t, err)
}

func TestDecide(t *testing.T) {
	min := Version{Major: 9, Minor: 2}
	old := Version{Major: 8, Minor: 4, Patch: 11}
	cur := Version{Major: 9, Minor: 3, Patch: 4}
	assert.Equal(t, Load, Decide(nil, min, 0))
	assert.Equal(t, Ready, Decide(&cur, min, 0))
	assert.Equal(t, Ready, Decide(&cur, min, 3))
	assert.Equal(t, Replace, Decide(&old, min, 0))
	assert.Equal(t, InUse, Decide(&old, min, 1))
	assert.Equal(t, "in-use", InUse.String())
	assert.Equal(t, "decision(9)", Decision(9).String())
}
