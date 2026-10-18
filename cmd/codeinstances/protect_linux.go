// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package main

import "syscall"

// protectSelf makes this process undumpable, so programs it runs as the same
// user can't read its environment (the token) or memory through /proc.
func protectSelf() error {
	const prSetDumpable = 4
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetDumpable, 0, 0); errno != 0 {
		return errno
	}
	return nil
}
