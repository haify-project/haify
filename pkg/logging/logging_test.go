package logging

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func TestParseLevel(t *testing.T) {
	assert.Equal(t, zapcore.DebugLevel, ParseLevel("DEBUG", zapcore.InfoLevel))
	assert.Equal(t, zapcore.WarnLevel, ParseLevel(" warning ", zapcore.InfoLevel))
	assert.Equal(t, zapcore.ErrorLevel, ParseLevel("error", zapcore.InfoLevel))
	assert.Equal(t, zapcore.InfoLevel, ParseLevel("", zapcore.InfoLevel))
	assert.Equal(t, zapcore.WarnLevel, ParseLevel("loud", zapcore.WarnLevel))
}

func TestInstallReplacesGlobal(t *testing.T) {
	restore := zap.ReplaceGlobals(zap.NewNop())
	defer restore()
	l := zap.NewExample()
	assert.Same(t, l, Install(l))
	assert.Same(t, l, zap.L())
}
