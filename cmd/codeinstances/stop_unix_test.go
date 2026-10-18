// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/codeinstances/codeinstances"
	"github.com/codeinstances/codeinstances/instance"
)

func TestStoppingTheStoreStopsItsRuns(t *testing.T) {
	dir := t.TempDir()
	key := filepath.Join(dir, "ci")
	cli(t, "keygen", "-o", key)
	mark := filepath.Join(dir, "pid")
	file := filepath.Join(dir, "sleep.instance")
	t.Setenv("CI_CMD_CHILD", "sleep")
	t.Setenv("CI_CMD_SLEEP", "10ms")
	t.Setenv("CI_CMD_MARK", mark)
	cli(t, append([]string{"record", "-q", "-sign", key + ".key", "-env", "CI_CMD_CHILD", "-env", "CI_CMD_SLEEP", "-env", "CI_CMD_MARK", "-o", file, "--"}, childArgs()...)...)
	os.Unsetenv("CI_CMD_CHILD")
	// Make the stored run sleep long: edit, then sign again.
	inst, _ := instance.Load(file)
	inst.Command.Env["CI_CMD_SLEEP"] = "60s"
	inst.Signatures = nil
	k, _ := codeinstances.ReadPrivateKey(key + ".key")
	inst.Sign(k, "", time.Now())
	instance.Save(file, inst)
	os.Remove(mark)

	cmd := exec.Command(os.Args[0], "serve", "-dir", filepath.Join(dir, "store"), "-addr", "127.0.0.1:0", "-run", "-run-trust", key+".pub")
	cmd.Env = append(os.Environ(), "CI_CLI=1", "CODEINSTANCES_TOKEN=sesame")
	pipe, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	rd := bufio.NewReader(pipe)
	line, _ := rd.ReadString('\n')
	_, addr, _ := strings.Cut(line, " at ")
	addr = strings.TrimSpace(addr)
	go io.Copy(io.Discard, rd)
	if _, e, code := cli(t, "push", "-to", addr, "-token", "sesame", file); code != 0 {
		t.Fatal(e)
	}
	stored, _ := instance.Load(file)
	go cli(t, "run", "-on", addr, "-token", "sesame", stored.ID)
	var pid int
	for i := 0; i < 100 && pid == 0; i++ {
		time.Sleep(50 * time.Millisecond)
		b, _ := os.ReadFile(mark)
		pid, _ = strconv.Atoi(string(b))
	}
	if pid == 0 {
		t.Fatal("the run never started")
	}
	cmd.Process.Signal(syscall.SIGTERM)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("serve didn't stop cleanly: %v", err)
	}
	// By the time serve has returned, the run is gone.
	if syscall.Kill(pid, 0) == nil {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Fatal("the run outlived the store")
	}
}
