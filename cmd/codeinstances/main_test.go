// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
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
	"github.com/codeinstances/codeinstances/store"
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
	case "answers":
		// Fails when the answer to /2 is "bad".
		c := hook.Client()
		code := 0
		for i := 1; i <= 3; i++ {
			resp, err := c.Get(fmt.Sprintf("http://plain.test/%d", i))
			if err != nil {
				fmt.Println("error", err)
				os.Exit(2)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			fmt.Println(i, string(b))
			if i == 2 && string(b) == "bad" {
				code = 1
			}
		}
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

func TestShowJSONIsTheFileItself(t *testing.T) {
	file := filepath.Join("..", "..", "docs", "example.instance")
	data, _ := os.ReadFile(file)
	out, _, code := cli(t, "show", "-json", file)
	if code != 0 || out != string(data) {
		t.Fatal("show -json changed the file")
	}
}

func TestDiff(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.instance")
	b := filepath.Join(dir, "b.instance")
	t.Setenv("CI_CMD_CHILD", "hello")
	t.Setenv("CI_CMD_EXIT", "0")
	cli(t, append([]string{"record", "-q", "-o", a, "--"}, childArgs()...)...)
	t.Setenv("CI_CMD_EXIT", "3")
	cli(t, append([]string{"record", "-q", "-o", b, "--"}, childArgs()...)...)

	out, _, code := cli(t, "diff", a, a)
	if code != 0 || !strings.Contains(out, "codeinstances: the runs are the same") || !strings.Contains(out, "calls  0 the same") {
		t.Fatalf("%d\n%s", code, out)
	}
	out, _, code = cli(t, "diff", a, b)
	for _, want := range []string{"a  " + a, "b  " + b, "ended: a exit 0, b exit 3", "stdout line 1: a \"hello ", "speed  a "} {
		if code != 1 || !strings.Contains(out, want) {
			t.Fatalf("diff lacks %q (exit %d):\n%s", want, code, out)
		}
	}
	// The random value differs, but only its count is compared by default.
	if strings.Contains(out, "random value 1") {
		t.Fatal(out)
	}
	if out, _, _ := cli(t, "diff", "-values", a, b); !strings.Contains(out, "random value 1: a ") {
		t.Fatal(out)
	}
	out, _, code = cli(t, "diff", "-json", a, b)
	var c codeinstances.Comparison
	if code != 1 || json.Unmarshal([]byte(out), &c) != nil || c.Same || c.A.Ended != "exit 0" || c.B.Ended != "exit 3" {
		t.Fatalf("%d %s", code, out)
	}

	data, _ := os.ReadFile(b)
	edited := filepath.Join(dir, "edited.instance")
	os.WriteFile(edited, bytes.Replace(data, []byte(`"exit_code": 3`), []byte(`"exit_code": 0`), 1), 0o600)
	for _, args := range [][]string{{"diff"}, {"diff", a}, {"diff", a, b, a}, {"diff", a, edited}, {"diff", a, filepath.Join(dir, "missing")}, {"diff", "-nope", a, b}} {
		if _, _, code := cli(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

// answersFile writes a hand-made recording of the "answers" child.
func answersFile(t *testing.T, path string, exit int, answers ...string) {
	t.Helper()
	var printed strings.Builder
	for i, a := range answers {
		fmt.Fprintln(&printed, i+1, a)
	}
	inst := &instance.Instance{Tool: "test", Command: instance.Command{Argv: childArgs()},
		Result: instance.Result{ExitCode: exit, Stdout: instance.NewBody([]byte(printed.String()))}}
	for i, a := range answers {
		inst.Calls = append(inst.Calls, instance.Call{Seq: i + 1, Kind: instance.KindHTTP,
			Request:  instance.Request{Method: "GET", URL: fmt.Sprintf("http://plain.test/%d", i+1)},
			Response: &instance.Response{Status: 200, Body: instance.NewBody([]byte(a))}})
	}
	if err := instance.Save(path, inst); err != nil {
		t.Fatal(err)
	}
}

func TestShrinkAndTest(t *testing.T) {
	dir := t.TempDir()
	pass := filepath.Join(dir, "pass.instance")
	fail := filepath.Join(dir, "fail.instance")
	answersFile(t, pass, 0, "good", "good", "good")
	answersFile(t, fail, 1, "bad", "bad", "bad")
	t.Setenv("CI_CMD_CHILD", "answers")

	out := filepath.Join(dir, "shrunk.instance")
	o, e, code := cli(t, "shrink", "-pass", pass, "-o", out, fail)
	for _, want := range []string{"3 answers differ between the runs; the failure (exit 1) needs 1 of them (a is the passing run, b the failing one):", "GET http://plain.test/2 (failing call 2): response body line 1: a \"good\", b \"bad\"", "codeinstances: kept " + out} {
		if code != 0 || !strings.Contains(o, want) {
			t.Fatalf("lacks %q (exit %d):\n%s\n%s", want, code, o, e)
		}
	}
	if !strings.Contains(e, "codeinstances: run 1 with 3 of 3 failing answers: fails (exit 1)") {
		t.Fatal(e)
	}
	shrunk, err := instance.Load(out)
	if err != nil || len(shrunk.Calls) != 3 || string(shrunk.Calls[0].Response.Body.MustBytes()) != "good" || string(shrunk.Calls[1].Response.Body.MustBytes()) != "bad" {
		t.Fatal(err)
	}
	o, _, code = cli(t, "shrink", "-json", "-q", "-pass", pass, "-o", out, fail)
	var res struct {
		Needed []string `json:"needed"`
		File   string   `json:"file"`
		ID     string   `json:"id"`
	}
	if code != 0 || json.Unmarshal([]byte(o), &res) != nil || len(res.Needed) != 1 || res.File != out || !strings.HasPrefix(res.ID, "sha256:") {
		t.Fatalf("%d %s", code, o)
	}
	// Nothing to shrink: two passing runs.
	if _, e, code := cli(t, "shrink", "-q", "-pass", pass, "-o", out, pass); code != 1 || !strings.Contains(e, "the failing run exited 0") {
		t.Fatalf("%d %s", code, e)
	}
	for _, args := range [][]string{{"shrink", fail}, {"shrink", "-pass", pass}, {"shrink", "-pass", pass, "-match", "(", fail}, {"shrink", "-pass", filepath.Join(dir, "none"), fail}} {
		if _, _, code := cli(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}

	// As regression tests: the passing run replays, the failing one still fails.
	tests := filepath.Join(dir, "tests")
	os.Mkdir(tests, 0o700)
	os.Rename(pass, filepath.Join(tests, "a-pass.instance"))
	os.Rename(out, filepath.Join(tests, "b-bug.instance"))
	os.WriteFile(filepath.Join(tests, "notes.txt"), []byte("not an instance"), 0o600)
	o, _, code = cli(t, "test", tests)
	if code != 1 || !strings.Contains(o, "ok    "+filepath.Join(tests, "a-pass.instance")+"  matched") ||
		!strings.Contains(o, "FAIL  "+filepath.Join(tests, "b-bug.instance")) || !strings.Contains(o, "ended exit 1, want exit 0 (recorded exit 1)") ||
		!strings.Contains(o, "codeinstances: 1 of 2 tests passed") {
		t.Fatalf("%d\n%s", code, o)
	}
	// A fixed program passes both.
	t.Setenv("CI_CMD_CHILD", "hello")
	t.Setenv("CI_CMD_EXIT", "0")
	o, _, code = cli(t, append([]string{"test", "-expect", "pass", "-v", tests, "--"}, childArgs()...)...)
	if code != 0 || !strings.Contains(o, "exit 0 (recorded exit 1)") || !strings.Contains(o, "note: ") || !strings.Contains(o, "2 of 2 tests passed") {
		t.Fatalf("%d\n%s", code, o)
	}
	o, _, code = cli(t, "test", "-json", "-expect", "pass", tests, "--", childArgs()[0], childArgs()[1])
	var results []map[string]any
	if code != 0 || json.Unmarshal([]byte(o), &results) != nil || len(results) != 2 || results[0]["passed"] != true {
		t.Fatalf("%d %s", code, o)
	}
	for _, args := range [][]string{{"test"}, {"test", filepath.Join(dir, "none")}, {"test", "-expect", "maybe", tests}, {"test", t.TempDir()}} {
		if _, _, code := cli(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestGroup(t *testing.T) {
	dir := t.TempDir()
	save := func(name string, exit int, stderr string, at time.Time) {
		inst := &instance.Instance{Tool: "test", Created: at, Command: instance.Command{Argv: []string{"prog"}},
			Result: instance.Result{ExitCode: exit, Stderr: instance.NewBody([]byte(stderr))}}
		if err := instance.Save(filepath.Join(dir, name), inst); err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Date(2026, 10, 18, 6, 0, 0, 0, time.UTC)
	for i := 0; i < 7; i++ {
		save(fmt.Sprintf("timeout-%d.instance", i), 1, fmt.Sprintf("request %d timed out after %dms\n", 100+i, 3000+i), t0.Add(time.Duration(i)*time.Minute))
	}
	save("other.instance", 2, "panic: nil map\n\ngoroutine 1 [running]:\nmain.main()\n\t/app/main.go:9 +0x1\n", t0)
	save("fine.instance", 0, "", t0)

	o, _, code := cli(t, "group", dir)
	for _, want := range []string{
		"codeinstances: 8 failing runs in 2 groups (1 run passed)",
		"1. 7 runs  fp1:", "2026-10-18 06:00 UTC to 2026-10-18 06:06 UTC",
		"   ended  exit 1\n   error  request <n> timed out after <duration>\n",
		"... and 2 more (-v lists them)",
		"2. 1 run  fp1:", "   error  panic: nil map\n          at main.main\n",
	} {
		if code != 0 || !strings.Contains(o, want) {
			t.Fatalf("lacks %q (exit %d):\n%s", want, code, o)
		}
	}
	if o, _, _ := cli(t, "group", "-v", dir); !strings.Contains(o, "timeout-6.instance") {
		t.Fatal(o)
	}
	o, _, code = cli(t, "group", "-json", dir)
	var g codeinstances.Grouping
	if code != 0 || json.Unmarshal([]byte(o), &g) != nil || g.Failing != 8 || g.Passed != 1 || len(g.Groups[0].Runs) != 7 {
		t.Fatalf("%d %s", code, o)
	}
	o, _, _ = cli(t, "show", filepath.Join(dir, "other.instance"))
	if !strings.Contains(o, "failure   fp1:") || !strings.Contains(o, "exit 2 | panic: nil map / at main.main") {
		t.Fatal(o)
	}
	if o, _, _ := cli(t, "show", filepath.Join(dir, "fine.instance")); strings.Contains(o, "failure") {
		t.Fatal(o)
	}
	for _, args := range [][]string{{"group"}, {"group", filepath.Join(dir, "none")}, {"group", t.TempDir()}, {"group", "-x", dir}} {
		if _, _, code := cli(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestPushPullReport(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "store"))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(&store.Server{Store: st, Token: "sesame"})
	defer ts.Close()
	file := filepath.Join(dir, "bug.instance")
	inst := &instance.Instance{Tool: "test", Command: instance.Command{Argv: []string{"prog"}},
		Result: instance.Result{ExitCode: 1, Stderr: instance.NewBody([]byte("error: disk full\n"))}}
	if err := instance.Save(file, inst); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEINSTANCES_STORE", ts.URL)
	t.Setenv("CODEINSTANCES_TOKEN", "sesame")

	o, e, code := cli(t, "push", file)
	if code != 0 || !strings.Contains(o, file+"  "+inst.ID+"  "+ts.URL+"/v1/instances/"+inst.ID+" (new)") {
		t.Fatalf("%d %s %s", code, o, e)
	}
	if o, _, _ := cli(t, "push", dir); !strings.Contains(o, "(already there)") {
		t.Fatal(o)
	}
	o, _, code = cli(t, "push", "-json", file)
	var pushed []map[string]any
	if code != 0 || json.Unmarshal([]byte(o), &pushed) != nil || pushed[0]["id"] != inst.ID || pushed[0]["created"] != false {
		t.Fatal(o)
	}

	out := filepath.Join(dir, "pulled.instance")
	hex := strings.TrimPrefix(inst.ID, "sha256:")
	o, _, code = cli(t, "pull", "-o", out, hex[:12])
	if code != 0 || !strings.Contains(o, "codeinstances: pulled "+out) {
		t.Fatal(code, o)
	}
	a, _ := os.ReadFile(file)
	b, _ := os.ReadFile(out)
	if !bytes.Equal(a, b) {
		t.Fatal("pulled a different file")
	}
	wd, _ := os.Getwd()
	os.Chdir(dir)
	_, _, code = cli(t, "pull", inst.ID)
	os.Chdir(wd)
	if _, err := os.Stat(filepath.Join(dir, hex[:12]+".instance")); code != 0 || err != nil {
		t.Fatal("the default file name", code, err)
	}

	o, _, code = cli(t, "report", file)
	for _, want := range []string{"## exit 1: error: disk full\n", "codeinstances pull -from " + ts.URL + " -o bug.instance " + inst.ID, "codeinstances replay bug.instance"} {
		if code != 0 || !strings.Contains(o, want) {
			t.Fatalf("report lacks %q:\n%s", want, o)
		}
	}
	t.Setenv("CODEINSTANCES_STORE", "")
	if o, _, _ := cli(t, "report", file); strings.Contains(o, "pull") {
		t.Fatal(o)
	}

	for _, args := range [][]string{
		{"push", file}, // no store
		{"pull", hex[:12]},
		{"push", "-to", ts.URL, "-token", "wrong", file},
		{"pull", "-from", ts.URL, "nothex"},
		{"pull", "-from", ts.URL, strings.Repeat("0", 64)},
		{"push", "-to", ts.URL, filepath.Join(dir, "missing")},
		{"report"},
		{"report", filepath.Join(dir, "missing")},
		{"serve", "-addr", "0.0.0.0:0", "-token", ""},
		{"serve", "-addr", "nonsense"},
		{"serve", "extra"},
	} {
		if _, _, code := cli(t, args...); code != 2 {
			t.Errorf("%v: exit %d, want 2", args, code)
		}
	}
}

func TestServeAndStop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("SIGINT is a Unix test")
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "serve", "-dir", filepath.Join(dir, "store"), "-addr", "127.0.0.1:0")
	cmd.Env = append(os.Environ(), "CI_CLI=1", "CODEINSTANCES_TOKEN=")
	pipe, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line, _ := bufio.NewReader(pipe).ReadString('\n')
	_, addr, _ := strings.Cut(line, " at ")
	addr = strings.TrimSpace(addr)
	if !strings.HasPrefix(addr, "http://127.0.0.1:") {
		cmd.Process.Kill()
		t.Fatalf("%q", line)
	}
	go io.Copy(io.Discard, pipe)
	resp, err := http.Get(addr + "/v1/instances")
	if err != nil || resp.StatusCode != 200 {
		t.Fatal(err)
	}
	resp.Body.Close()
	cmd.Process.Signal(syscall.SIGINT)
	if err := cmd.Wait(); err != nil {
		t.Fatalf("serve didn't stop cleanly: %v", err)
	}
}
