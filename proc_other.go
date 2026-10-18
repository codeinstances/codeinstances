// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package codeinstances

import "os/exec"

// isolate does nothing here: on Windows a time limit stops the program but
// not processes it started.
func isolate(cmd *exec.Cmd) {}

func killGroup(cmd *exec.Cmd) {}
