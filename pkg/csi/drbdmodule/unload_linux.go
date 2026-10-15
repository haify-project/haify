//go:build linux

package drbdmodule

import "golang.org/x/sys/unix"

// deleteModule unloads a kernel module the way rmmod does: O_NONBLOCK makes
// the kernel refuse (EWOULDBLOCK/EBUSY) a module that is in use instead of
// waiting for it to become free.
func deleteModule(name string) error {
	return unix.DeleteModule(name, unix.O_NONBLOCK)
}
