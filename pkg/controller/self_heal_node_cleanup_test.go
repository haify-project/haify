package controller

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A stale encrypted volume's container is closed and its key destroyed before
// the volume is removed: an open container holds the LV, and a key left on
// the node would outlive the data it protects.
func TestStaleResourceCleanupClosesLUKSBeforeRemoving(t *testing.T) {
	script := staleResourceCleanup("db")
	if sh, err := exec.LookPath("bash"); err == nil {
		out, err := exec.Command(sh, "-n", "-c", script).CombinedOutput()
		require.NoError(t, err, string(out))
	}
	assert.Contains(t, script, `n="haify_$1_$2"`)
	assert.Contains(t, script, `cryptsetup close "$n"`)
	assert.Contains(t, script, `shred -u "`+luksKeyDir+`/$n.key"`)
	assert.Contains(t, script, `close_luks "${lv%%/*}" "${lv#*/}"`)
	assert.Less(t, strings.Index(script, `close_luks "${lv`), strings.Index(script, `lvremove -f "$lv"`))
}
