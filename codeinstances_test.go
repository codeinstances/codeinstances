// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package codeinstances

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/codeinstances/codeinstances/hook"
	"github.com/codeinstances/codeinstances/instance"
	"github.com/codeinstances/codeinstances/internal/ca"
)

// The test binary doubles as the recorded program: CI_TEST_CHILD names what
// it should do.
func TestMain(m *testing.M) {
	if what := os.Getenv("CI_TEST_CHILD"); what != "" {
		os.Exit(child(what))
	}
	os.Exit(m.Run())
}

func child(what string) int {
	c := hook.Client()
	c.Timeout = 20 * time.Second
	get := func(u string) {
		resp, err := c.Get(u)
		if err != nil {
			fmt.Println("error", err)
			return
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		fmt.Println(resp.StatusCode, string(b))
	}
	switch what {
	case "web", "extra", "fewer", "morevalues":
		fmt.Println("at", hook.Now().Format(time.RFC3339Nano), "id", hook.Uint64())
		get("https://secure.test/a")
		if what != "fewer" {
			resp, err := c.Post("http://plain.test/b", "text/plain", strings.NewReader("payload"))
			if err == nil {
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				fmt.Println(resp.StatusCode, string(b))
			}
		}
		if what == "extra" {
			get("https://secure.test/extra")
		}
		if what == "morevalues" {
			fmt.Println(hook.Uint64())
		}
		fmt.Fprintln(os.Stderr, "done")
		if code := os.Getenv("CHILD_EXIT"); code != "" {
			var n int
			fmt.Sscan(code, &n)
			return n
		}
		return 0
	case "shrinkable":
		// Fails only when the answers to /3 and /5 both say "bad".
		bad := 0
		for i := 1; i <= 6; i++ {
			resp, err := c.Get(fmt.Sprintf("https://secure.test/%d", i))
			if err != nil {
				fmt.Println("error", err)
				return 2
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			fmt.Println(i, string(b))
			if (i == 3 || i == 5) && string(b) == "bad" {
				bad++
			}
		}
		if bad == 2 {
			get("https://secure.test/report")
			fmt.Fprintln(os.Stderr, "failed: both bad")
			return 1
		}
		return 0
	case "unhooked":
		fmt.Println("at", time.Now().Format(time.RFC3339Nano))
		return 0
	case "stderr-noise":
		fmt.Println("steady")
		fmt.Fprintln(os.Stderr, "noise", time.Now().UnixNano())
		return 0
	case "crash":
		panic("the child crashes")
	case "signal":
		p, _ := os.FindProcess(os.Getpid())
		p.Signal(os.Kill)
		time.Sleep(10 * time.Second)
		return 0
	case "sleep":
		time.Sleep(30 * time.Second)
		return 0
	case "env":
		fmt.Println("kept", os.Getenv("CI_KEPT"), "other", os.Getenv("CI_OTHER"))
		return 0
	case "bigout":
		chunk := bytes.Repeat([]byte("o"), 1<<20)
		for i := 0; i < 20; i++ {
			os.Stdout.Write(chunk)
		}
		return 0
	case "binary":
		os.Stdout.Write([]byte{0xff, 0x00, 0xfe})
		return 0
	}
	return 99
}

type world struct {
	tls, plain *httptest.Server
	caPEM      []byte
	hits       atomic.Int64
}

func newWorld(t *testing.T) *world {
	t.Helper()
	auth, err := ca.New("servers")
	if err != nil {
		t.Fatal(err)
	}
	w := &world{caPEM: auth.CertPEM()}
	h := http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		n := w.hits.Add(1)
		b, _ := io.ReadAll(r.Body)
		fmt.Fprintf(rw, "%s %s n=%d t=%d body=%s", r.Method, r.URL.Path, n, time.Now().UnixNano(), b)
	})
	w.plain = httptest.NewServer(h)
	w.tls = httptest.NewUnstartedServer(h)
	cert, _ := auth.Issue("secure.test")
	w.tls.TLS = &tls.Config{Certificates: []tls.Certificate{*cert}}
	w.tls.StartTLS()
	t.Cleanup(func() { w.tls.Close(); w.plain.Close() })
	return w
}

func (w *world) down() {
	w.tls.Close()
	w.plain.Close()
}

func (w *world) record(t *testing.T, what string, extra ...func(*RecordOptions)) *instance.Instance {
	t.Helper()
	opts := RecordOptions{
		Argv:        []string{os.Args[0], "-test.run=^$"},
		Resolve:     map[string]string{"secure.test": w.tls.Listener.Addr().String(), "plain.test": w.plain.Listener.Addr().String()},
		UpstreamCAs: [][]byte{w.caPEM},
		Timeout:     time.Minute,
	}
	for _, f := range extra {
		f(&opts)
	}
	t.Setenv("CI_TEST_CHILD", what)
	inst, err := Record(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	// Through the file and back, as the command does.
	path := filepath.Join(t.TempDir(), "x.instance")
	if err := instance.Save(path, inst); err != nil {
		t.Fatal(err)
	}
	loaded, err := instance.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func replayAs(t *testing.T, inst *instance.Instance, what string, opts ReplayOptions) *Report {
	t.Helper()
	t.Setenv("CI_TEST_CHILD", what)
	if opts.Timeout == 0 {
		opts.Timeout = time.Minute
	}
	rep, err := Replay(context.Background(), inst, opts)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestRecordAndReplayMatchOffline(t *testing.T) {
	w := newWorld(t)
	inst := w.record(t, "web")
	if len(inst.Calls) != 2 || len(inst.Values) != 2 || inst.Result.ExitCode != 0 {
		t.Fatalf("calls %d values %d exit %d", len(inst.Calls), len(inst.Values), inst.Result.ExitCode)
	}
	out := string(inst.Result.Stdout.MustBytes())
	if !strings.Contains(out, "200 GET /a n=") || !strings.Contains(out, "200 POST /b") || !strings.Contains(out, "body=payload") {
		t.Fatalf("stdout %q", out)
	}
	if inst.Command.Dir == "" || inst.Tool != "codeinstances "+Version {
		t.Fatalf("%+v", inst.Command)
	}
	w.down()
	hits := w.hits.Load()
	for i := 0; i < 3; i++ {
		rep := replayAs(t, inst, "web", ReplayOptions{})
		if !rep.Matched {
			t.Fatalf("replay %d: %v", i, rep.Divergences)
		}
		if rep.CallsServed != 2 || rep.ValuesUsed != 2 || rep.Calls != 2 || rep.Values != 2 {
			t.Fatalf("%+v", rep)
		}
	}
	if w.hits.Load() != hits {
		t.Fatal("replay reached the servers")
	}
}

func TestExitCodeIsRecordedAndCompared(t *testing.T) {
	w := newWorld(t)
	t.Setenv("CHILD_EXIT", "7")
	inst := w.record(t, "web")
	if inst.Result.ExitCode != 7 {
		t.Fatal(inst.Result.ExitCode)
	}
	if rep := replayAs(t, inst, "web", ReplayOptions{}); !rep.Matched {
		t.Fatal(rep.Divergences)
	}
	t.Setenv("CHILD_EXIT", "0")
	rep := replayAs(t, inst, "web", ReplayOptions{})
	if rep.Matched || !strings.Contains(strings.Join(rep.Divergences, ";"), "exit code: recorded 7, replayed 0") {
		t.Fatal(rep.Divergences)
	}
}

func TestChangedProgramsAreCaught(t *testing.T) {
	w := newWorld(t)
	inst := w.record(t, "web")
	w.down()
	cases := map[string]string{
		"extra":      "GET https://secure.test/extra isn't in the recording",
		"fewer":      "recorded call 2 (POST http://plain.test/b) was never made",
		"morevalues": "asked for 1 more random values than the 1 recorded",
		"unhooked":   "the program used 0 of 1 recorded clock values",
	}
	for what, want := range cases {
		rep := replayAs(t, inst, what, ReplayOptions{})
		if rep.Matched || !strings.Contains(strings.Join(rep.Divergences, "\n"), want) {
			t.Errorf("%s: want %q in %q", what, want, rep.Divergences)
		}
	}
}

func TestStderrComparisonCanBeSkipped(t *testing.T) {
	w := newWorld(t)
	inst := w.record(t, "stderr-noise")
	rep := replayAs(t, inst, "stderr-noise", ReplayOptions{})
	if rep.Matched || !strings.HasPrefix(rep.Divergences[0], "stderr line 1") {
		t.Fatal(rep.Divergences)
	}
	if rep := replayAs(t, inst, "stderr-noise", ReplayOptions{IgnoreStderr: true}); !rep.Matched {
		t.Fatal(rep.Divergences)
	}
}

func TestCrashesAreRecordedAndReplayed(t *testing.T) {
	w := newWorld(t)
	inst := w.record(t, "crash")
	if inst.Result.ExitCode != 2 || !strings.Contains(string(inst.Result.Stderr.MustBytes()), "the child crashes") {
		t.Fatalf("%+v", inst.Result)
	}
	// Panic output includes goroutine addresses that change; the exit code
	// and stdout still match.
	if rep := replayAs(t, inst, "crash", ReplayOptions{IgnoreStderr: true}); !rep.Matched {
		t.Fatal(rep.Divergences)
	}
}

func TestKilledProgramIsRecorded(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no signals on Windows")
	}
	w := newWorld(t)
	inst := w.record(t, "signal")
	if inst.Result.Signal != syscall.SIGKILL.String() || inst.Result.ExitCode != -1 {
		t.Fatalf("%+v", inst.Result)
	}
	if rep := replayAs(t, inst, "signal", ReplayOptions{}); !rep.Matched {
		t.Fatal(rep.Divergences)
	}
	rep := replayAs(t, inst, "web", ReplayOptions{})
	if rep.Matched || !strings.Contains(rep.Divergences[0], "signal: recorded") {
		t.Fatal(rep.Divergences)
	}
}

func TestTimeoutIsRecorded(t *testing.T) {
	w := newWorld(t)
	start := time.Now()
	inst := w.record(t, "sleep", func(o *RecordOptions) { o.Timeout = 300 * time.Millisecond })
	if !inst.Result.TimedOut || time.Since(start) > 10*time.Second {
		t.Fatalf("%+v after %v", inst.Result, time.Since(start))
	}
	rep := replayAs(t, inst, "sleep", ReplayOptions{Timeout: 300 * time.Millisecond})
	if !rep.Matched {
		t.Fatal(rep.Divergences)
	}
}

func TestKeptEnvironmentIsSetOnReplay(t *testing.T) {
	w := newWorld(t)
	t.Setenv("CI_KEPT", "recorded-value")
	t.Setenv("CI_OTHER", "recorded-other")
	inst := w.record(t, "env", func(o *RecordOptions) { o.KeepEnv = []string{"CI_KEPT", "CI_MISSING"} })
	if inst.Command.Env["CI_KEPT"] != "recorded-value" || len(inst.Command.Env) != 1 {
		t.Fatal(inst.Command.Env)
	}
	t.Setenv("CI_KEPT", "changed")
	t.Setenv("CI_OTHER", "changed")
	rep := replayAs(t, inst, "env", ReplayOptions{})
	if rep.Matched || len(rep.Divergences) != 1 || !strings.Contains(rep.Divergences[0], `replayed "kept recorded-value other changed"`) {
		t.Fatal(rep.Divergences)
	}
}

func TestLargeAndBinaryOutput(t *testing.T) {
	w := newWorld(t)
	inst := w.record(t, "bigout")
	if !inst.Result.StdoutTruncated || len(inst.Result.Stdout.MustBytes()) != MaxOutput {
		t.Fatalf("truncated %v, %d bytes", inst.Result.StdoutTruncated, len(inst.Result.Stdout.MustBytes()))
	}
	if rep := replayAs(t, inst, "bigout", ReplayOptions{}); !rep.Matched {
		t.Fatal(rep.Divergences)
	}
	bin := w.record(t, "binary")
	if !bytes.Equal(bin.Result.Stdout.MustBytes(), []byte{0xff, 0, 0xfe}) || bin.Result.Stdout.Base64 == "" {
		t.Fatalf("%+v", bin.Result.Stdout)
	}
	if rep := replayAs(t, bin, "binary", ReplayOptions{}); !rep.Matched {
		t.Fatal(rep.Divergences)
	}
}

func TestReplayAnotherCommand(t *testing.T) {
	w := newWorld(t)
	inst := w.record(t, "web")
	w.down()
	// The same program under another path still matches.
	dir := t.TempDir()
	copyPath := filepath.Join(dir, "copy"+filepath.Ext(os.Args[0]))
	data, _ := os.ReadFile(os.Args[0])
	os.WriteFile(copyPath, data, 0o755)
	t.Setenv("CI_TEST_CHILD", "web")
	rep, err := Replay(context.Background(), inst, ReplayOptions{Argv: []string{copyPath, "-test.run=^$"}})
	if err != nil {
		t.Fatal(err)
	}
	if !rep.Matched {
		t.Fatal(rep.Divergences)
	}
}

func TestOutputIsPassedThrough(t *testing.T) {
	w := newWorld(t)
	var out, errOut bytes.Buffer
	w.record(t, "web", func(o *RecordOptions) { o.Stdout, o.Stderr = &out, &errOut })
	if !strings.Contains(out.String(), "200 GET /a") || errOut.String() != "done\n" {
		t.Fatalf("%q %q", out.String(), errOut.String())
	}
}

func TestErrors(t *testing.T) {
	if _, err := Record(context.Background(), RecordOptions{}); err == nil {
		t.Fatal("recorded nothing")
	}
	if _, err := Record(context.Background(), RecordOptions{Argv: []string{filepath.Join(t.TempDir(), "no-such-program")}}); err == nil {
		t.Fatal("recorded a missing program")
	}
	if _, err := Record(context.Background(), RecordOptions{Argv: []string{"x"}, UpstreamCAs: [][]byte{[]byte("not pem")}}); err == nil {
		t.Fatal("accepted a bad CA")
	}
	if _, err := Replay(context.Background(), &instance.Instance{}, ReplayOptions{}); err == nil {
		t.Fatal("replayed an empty instance")
	}
}

func TestCompareOutput(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"x\ny\n", "x\ny\n", ""},
		{"x\ny\n", "x\nz\n", `stdout line 2: recorded "y", replayed "z"`},
		{"x\n", "x\ny\n", `stdout line 2: recorded "", replayed "y"`},
		{"x", "x\ny", `stdout line 2: the replay printed "y", which the recording didn't`},
		{"x\ny", "x", `stdout line 2: the recording printed "y", which the replay didn't`},
	}
	for _, c := range cases {
		if got := compareOutput("stdout", []byte(c.a), []byte(c.b)); got != c.want {
			t.Errorf("%q vs %q: %q, want %q", c.a, c.b, got, c.want)
		}
	}
	if got := compareOutput("x", []byte(strings.Repeat("a", 200)), []byte("b")); !strings.Contains(got, "...") {
		t.Error(got)
	}
}

func TestEnvironmentForTheProgram(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://outer:1")
	t.Setenv("no_proxy", "everything")
	t.Setenv("CI_KEEP_ME", "yes")
	env := environment(runSpec{mode: "replay", proxy: "http://127.0.0.1:9", work: "/w", env: map[string]string{"CI_KEEP_ME": "replayed", "NO_PROXY": "*", "CODEINSTANCES_MODE": "record"}}, "/w/ca.pem")
	got := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		if _, dup := got[k]; dup {
			t.Fatalf("%s twice", k)
		}
		got[k] = v
	}
	if got["HTTPS_PROXY"] != "http://127.0.0.1:9" || got["https_proxy"] != "http://127.0.0.1:9" {
		t.Fatal("proxy not set")
	}
	// No way around the proxy, from the environment or a kept variable.
	if got["no_proxy"] != "" || got["NO_PROXY"] != "" && runtime.GOOS != "windows" {
		t.Fatalf("no_proxy %q, NO_PROXY %q", got["no_proxy"], got["NO_PROXY"])
	}
	if got["CI_KEEP_ME"] != "replayed" || got[hook.EnvMode] != "replay" || got["SSL_CERT_FILE"] != "/w/ca.pem" || got[hook.EnvValuesIn] == "" {
		t.Fatal(got)
	}
}

func TestDedupeKeepsDriveEntries(t *testing.T) {
	got := dedupe([]string{"=C:=C:\\a", "=D:=D:\\b", "A=1", "A=2"})
	if strings.Join(got, "|") != "=C:=C:\\a|=D:=D:\\b|A=2" {
		t.Fatal(got)
	}
}
