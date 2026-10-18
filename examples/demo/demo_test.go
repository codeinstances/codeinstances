// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestDemo builds the command and the demo and runs the demo's checks.
func TestDemo(t *testing.T) {
	if testing.Short() {
		t.Skip("builds two binaries")
	}
	dir := t.TempDir()
	exe := ""
	if runtime.GOOS == "windows" {
		exe = ".exe"
	}
	cli := filepath.Join(dir, "codeinstances"+exe)
	demo := filepath.Join(dir, "codeinstances-demo"+exe)
	for out, pkg := range map[string]string{cli: "../../cmd/codeinstances", demo: "."} {
		build := exec.Command("go", "build", "-o", out, pkg)
		build.Stderr = os.Stderr
		if err := build.Run(); err != nil {
			t.Fatalf("building %s: %v", pkg, err)
		}
	}
	cmd := exec.Command(demo, "-replays", "3")
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	var rep report
	if jerr := json.Unmarshal(out, &rep); jerr != nil {
		t.Fatalf("report: %v (%v)", jerr, err)
	}
	if err != nil || !rep.Passed {
		for _, c := range rep.Checks {
			if !c.Passed {
				t.Errorf("%s: %s", c.Name, c.Detail)
			}
		}
		t.Fatalf("demo failed: %v", err)
	}
	if rep.Calls != 3+parallel || rep.Values != 5 || len(rep.ReplayMS) != 3 {
		t.Fatalf("%+v", rep)
	}
}
