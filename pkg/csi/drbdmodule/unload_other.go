//go:build !linux

package drbdmodule

import "errors"

// deleteModule exists for builds on other systems (the CLI tools build on a
// Mac); kernel modules are only unloaded on Linux nodes.
func deleteModule(name string) error {
	return errors.New("unloading kernel module " + name + " is only possible on Linux")
}
