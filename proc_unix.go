// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package codeinstances

import (
	"os"
	"os/exec"
	"syscall"
)

// isolate puts the program in a process group of its own, so that a time
// limit or a cancel stops everything it started, not only the program.
func isolate(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != syscall.ESRCH {
			return err
		}
		return os.ErrProcessDone
	}
}

// killGroup stops whatever the program left running in its group.
func killGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
