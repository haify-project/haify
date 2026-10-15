package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRunDispatch(t *testing.T) {
	var out, errb bytes.Buffer
	assert.Equal(t, 2, run(nil, &out, &errb))
	assert.Contains(t, errb.String(), "usage: haify-csi")

	errb.Reset()
	assert.Equal(t, 2, run([]string{"frobnicate"}, &out, &errb))
	assert.Contains(t, errb.String(), `unknown command "frobnicate"`)

	assert.Equal(t, 0, run([]string{"--help"}, &out, &errb))
	assert.Contains(t, out.String(), "controller")

	errb.Reset()
	assert.Equal(t, 2, run([]string{"drbd-module"}, &out, &errb))
	assert.Contains(t, errb.String(), "usage: haify-csi drbd-module")
}
