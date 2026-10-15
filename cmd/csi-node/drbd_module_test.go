package main

import (
	"bytes"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

func TestRunDRBDModuleUsage(t *testing.T) {
	var out, errb bytes.Buffer
	assert.Equal(t, 2, runDRBDModule(zap.NewNop(), nil, &out, &errb))
	assert.Contains(t, errb.String(), "usage: csi-node drbd-module")

	errb.Reset()
	assert.Equal(t, 2, runDRBDModule(zap.NewNop(), []string{"frobnicate"}, &out, &errb))
	assert.Contains(t, errb.String(), `unknown command "frobnicate"`)

	errb.Reset()
	assert.Equal(t, 2, runDRBDModule(zap.NewNop(), []string{"check", "--min-version", "nine"}, &out, &errb))
	assert.Contains(t, errb.String(), "--min-version")

	errb.Reset()
	assert.Equal(t, 2, runDRBDModule(zap.NewNop(), []string{"check", "--no-such-flag"}, &out, &errb))
}

func TestRunDRBDModuleBringUpNeedsPrepare(t *testing.T) {
	var out, errb bytes.Buffer
	code := runDRBDModule(zap.NewNop(), []string{"bring-up", "--state-dir", t.TempDir()}, &out, &errb)
	assert.Equal(t, 1, code)
	assert.Contains(t, errb.String(), "prepare decision")
}
