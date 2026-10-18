// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/codeinstances/codeinstances"
	"github.com/codeinstances/codeinstances/hook"
	"github.com/codeinstances/codeinstances/instance"
)

// The test binary is also the command (CI_CLI=1) and the recorded program
// (CI_CMD_CHILD names what it does).
func TestMain(m *testing.M) {
	// The recorded program inherits CI_CLI; it is told apart by its arguments.
	if os.Getenv("CI_CLI") == "1" && len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-test.") {
		os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
	}
	switch os.Getenv("CI_CMD_CHILD") {
	case "":
	case "hello":
		fmt.Println("hello", hook.Uint64())
		code, _ := strconv.Atoi(os.Getenv("CI_CMD_EXIT"))
		os.Exit(code)
	case "different":
		fmt.Println("something else")
		os.Exit(0)
	case "sleep":
		if f := os.Getenv("CI_CMD_MARK"); f != "" {
			os.WriteFile(f, []byte(strconv.Itoa(os.Getpid())), 0o600)
		}
		d, _ := time.ParseDuration(os.Getenv("CI_CMD_SLEEP"))
		time.Sleep(d)
		fmt.Println("slept")
		os.Exit(0)
	default:
		os.Exit(99)
	}
	os.Exit(m.Run())
}

func cli(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(args, &out, &errOut)
	return out.String(), errOut.String(), code
}

func childArgs() []string { return []string{os.Args[0], "-test.run=^$"} }

func TestUsageAndVersion(t *testing.T) {
	if _, e, code := cli(t); code != 2 || !strings.Contains(e, "Usage:") {
		t.Fatal(code, e)
	}
	if o, _, code := cli(t, "help"); code != 0 || !strings.Contains(o, "codeinstances record") {
		t.Fatal(code)
	}
	if _, e, code := cli(t, "frobnicate"); code != 2 || !strings.Contains(e, `unknown command "frobnicate"`) {
		t.Fatal(code, e)
	}
	o, _, code := cli(t, "version")
	if code != 0 || !strings.HasPrefix(o, "codeinstances "+codeinstances.Version+" (format "+instance.Format) {
		t.Fatal(o)
	}
	o, _, code = cli(t, "version", "-json")
	var info map[string]any
	if code != 0 || json.Unmarshal([]byte(o), &info) != nil || info["version"] != codeinstances.Version {
		t.Fatal(o)
	}
	if _, _, code := cli(t, "version", "x"); code != 2 {
		t.Fatal(code)
	}
}

func TestRecordShowReplayVerify(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "hello.instance")
	t.Setenv("CI_CMD_CHILD", "hello")
	t.Setenv("CI_CMD_EXIT", "3")
	out, errOut, code := cli(t, append([]string{"record", "-o", file, "--"}, childArgs()...)...)
	if code != 3 {
		t.Fatalf("record exited %d, want the program's 3: %s", code, errOut)
	}
	if !strings.HasPrefix(out, "hello ") || !strings.Contains(errOut, "codeinstances: kept "+file) {
		t.Fatalf("%q %q", out, errOut)
	}

	out, _, code = cli(t, "show", file)
	for _, want := range []string{"instance  sha256:", "ended     exit 3", "calls     0", "values    1 (clock 0, random 1, bytes 0)"} {
		if code != 0 || !strings.Contains(out, want) {
			t.Fatalf("show lacks %q:\n%s", want, out)
		}
	}
	out, _, code = cli(t, "show", "-json", file)
	if _, err := instance.Parse([]byte(out)); code != 0 || err != nil {
		t.Fatal(code, err)
	}

	out, errOut, code = cli(t, "replay", file)
	if code != 0 || !strings.Contains(errOut, "replay matched (0 of 0 calls served, 1 of 1 values used)") || !strings.HasPrefix(out, "hello ") {
		t.Fatalf("%d %q %q", code, out, errOut)
	}
	out, _, code = cli(t, "replay", "-json", file)
	var rep codeinstances.Report
	if code != 0 || json.Unmarshal([]byte(out), &rep) != nil || !rep.Matched {
		t.Fatalf("%d %q", code, out)
	}

	t.Setenv("CI_CMD_CHILD", "different")
	_, errOut, code = cli(t, "replay", file)
	if code != 1 || !strings.Contains(errOut, "replay diverged") || !strings.Contains(errOut, "exit code: recorded 3, replayed 0") {
		t.Fatalf("%d %q", code, errOut)
	}
	t.Setenv("CI_CMD_CHILD", "hello")
	if _, _, code := cli(t, append([]string{"replay", "-q", file, "--"}, childArgs()...)...); code != 0 {
		t.Fatal("replay with an explicit command")
	}

	out, _, code = cli(t, "verify", file)
	if code != 0 || !strings.Contains(out, "intact") {
		t.Fatal(code, out)
	}
	data, _ := os.ReadFile(file)
	edited := filepath.Join(dir, "edited.instance")
	os.WriteFile(edited, bytes.Replace(data, []byte(`"exit_code": 3`), []byte(`"exit_code": 0`), 1), 0o600)
	if _, e, code := cli(t, "verify", edited); code != 1 || !strings.Contains(e, "changed after it was recorded") {
		t.Fatal(code, e)
	}
	if _, _, code := cli(t, "replay", edited); code != 2 {
		t.Fatal("replayed an edited file")
	}
}

func TestBadArguments(t *testing.T) {
	dir := t.TempDir()
	notInstance := filepath.Join(dir, "x.json")
	os.WriteFile(notInstance, []byte(`{"hello":1}`), 0o600)
	cases := [][]string{
		{"record"},
		{"record", "-o"},
		{"record", "-resolve", "nohost", "--", "x"},
		{"record", "-upstream-ca", filepath.Join(dir, "missing.pem"), "--", "x"},
		{"record", "-q", "stray", "--", "x"},
		{"record", "--", filepath.Join(dir, "no-such-program")},
		{"replay"},
		{"replay", "a", "b"},
		{"replay", filepath.Join(dir, "missing")},
		{"replay", notInstance},
		{"show"},
		{"show", notInstance},
		{"verify"},
		{"verify", filepath.Join(dir, "missing")},
		{"verify", notInstance},
	}
	for _, args := range cases {
		if _, _, code := cli(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
	if _, err := os.Stat("run.instance"); err == nil {
		t.Fatal("a failed record left run.instance behind")
	}
}

func TestRecordWithoutDoubleDash(t *testing.T) {
	file := filepath.Join(t.TempDir(), "a.instance")
	t.Setenv("CI_CMD_CHILD", "hello")
	t.Setenv("CI_CMD_EXIT", "0")
	args := append([]string{"record", "-q", "-o", file}, childArgs()...)
	if out, e, code := cli(t, args...); code != 0 || out != "" {
		t.Fatalf("%d %q %q", code, out, e)
	}
	if _, err := instance.Load(file); err != nil {
		t.Fatal(err)
	}
}

// startCLI runs the test binary as the codeinstances command in its own
// process, recording a sleeping child that writes its pid to mark.
func startCLI(t *testing.T, file, mark, sleep string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], append([]string{"record", "-q", "-o", file, "--"}, childArgs()...)...)
	cmd.Env = append(os.Environ(), "CI_CLI=1", "CI_CMD_CHILD=sleep", "CI_CMD_MARK="+mark, "CI_CMD_SLEEP="+sleep)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(20 * time.Second)
	for {
		if b, err := os.ReadFile(mark); err == nil && len(b) > 0 {
			return cmd
		}
		if time.Now().After(deadline) {
			cmd.Process.Kill()
			t.Fatal("the recorded program never started")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestKilledRecordLeavesNoFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGKILL is a Unix test")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "killed.instance")
	mark := filepath.Join(dir, "pid")
	cmd := startCLI(t, file, mark, "30s")
	cmd.Process.Signal(syscall.SIGKILL)
	cmd.Wait()
	// The orphaned program is ours to clean up.
	if b, err := os.ReadFile(mark); err == nil {
		if pid, err := strconv.Atoi(string(b)); err == nil {
			if p, err := os.FindProcess(pid); err == nil {
				p.Kill()
			}
		}
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.Name() != "pid" {
			t.Fatalf("left behind: %s", e.Name())
		}
	}
}

func TestInterruptReachesTheProgramNotTheRecorder(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGINT is a Unix test")
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "interrupted.instance")
	cmd := startCLI(t, file, filepath.Join(dir, "pid"), "800ms")
	cmd.Process.Signal(syscall.SIGINT)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("the recorder didn't finish: %v", err)
	}
	inst, err := instance.Load(file)
	if err != nil {
		t.Fatal(err)
	}
	if string(inst.Result.Stdout.MustBytes()) != "slept\n" {
		t.Fatalf("%q", inst.Result.Stdout.MustBytes())
	}
}

func TestQuoteArgs(t *testing.T) {
	got := strings.Join(quoteArgs([]string{"a", "b c", "", `d"e`}), " ")
	if got != `a "b c" "" "d\"e"` {
		t.Fatal(got)
	}
}
