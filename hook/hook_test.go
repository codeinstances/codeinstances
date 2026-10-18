// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package hook_test

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codeinstances/codeinstances/hook"
	"github.com/codeinstances/codeinstances/internal/ca"
)

// The test binary runs itself as the hooked program, since the hook reads
// its mode once per process.
func TestMain(m *testing.M) {
	if os.Getenv("HOOK_HELPER") == "1" {
		helper()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func helper() {
	fmt.Println(hook.Active())
	fmt.Println(hook.Now().Format(time.RFC3339Nano))
	fmt.Println(hook.Uint64())
	fmt.Println(hook.Int63() >= 0)
	fmt.Println(hook.Intn(1000))
	f := hook.Float64()
	fmt.Println(f >= 0 && f < 1, f)
	b := make([]byte, 12)
	n, err := hook.Read(b)
	fmt.Printf("%d %v %x\n", n, err, b)
	fmt.Println(hook.Since(time.Unix(0, 0)) > 0)
}

func runHelper(t *testing.T, env ...string) []string {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "HOOK_HELPER=1")
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("helper: %v", err)
	}
	return strings.Split(strings.TrimSpace(string(out)), "\n")
}

func readLines(t *testing.T, path string) []hook.Line {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []hook.Line
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var l hook.Line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			t.Fatal(err)
		}
		out = append(out, l)
	}
	return out
}

func TestOutsideCodeinstancesTheValuesAreReal(t *testing.T) {
	out := runHelper(t)
	if out[0] != "false" {
		t.Fatal("active without codeinstances")
	}
	ts, err := time.Parse(time.RFC3339Nano, out[1])
	if err != nil || time.Since(ts) > time.Minute || time.Since(ts) < 0 {
		t.Fatalf("clock %q", out[1])
	}
	if out[3] != "true" || !strings.HasPrefix(out[5], "true") || !strings.HasPrefix(out[6], "12 <nil>") {
		t.Fatal(out)
	}
	if a, b := runHelper(t), runHelper(t); a[2] == b[2] {
		t.Fatal("random numbers repeat between runs")
	}
}

func TestRecordThenReplay(t *testing.T) {
	dir := t.TempDir()
	rec := filepath.Join(dir, "rec")
	first := runHelper(t, "CODEINSTANCES_MODE=record", "CODEINSTANCES_VALUES_OUT="+rec)
	lines := readLines(t, rec)
	kinds := []string{}
	for _, l := range lines {
		kinds = append(kinds, l.Kind)
		if l.Live {
			t.Fatal("live flag while recording")
		}
	}
	if strings.Join(kinds, ",") != "clock,random,random,random,random,bytes,clock" {
		t.Fatalf("kinds %v", kinds)
	}
	if first[0] != "true" {
		t.Fatal("not active")
	}
	time.Sleep(10 * time.Millisecond)
	for i := 0; i < 3; i++ {
		out := filepath.Join(dir, fmt.Sprint("rep", i))
		again := runHelper(t, "CODEINSTANCES_MODE=replay", "CODEINSTANCES_VALUES_IN="+rec, "CODEINSTANCES_VALUES_OUT="+out)
		if strings.Join(again, "\n") != strings.Join(first, "\n") {
			t.Fatalf("replay %d:\n%s\nrecorded:\n%s", i, strings.Join(again, "\n"), strings.Join(first, "\n"))
		}
		for _, l := range readLines(t, out) {
			if l.Live {
				t.Fatal("live value in a full replay")
			}
		}
	}
}

func TestReplayRunsOutAndSaysSo(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in")
	os.WriteFile(in, []byte(`{"kind":"clock","value":"2020-01-02T03:04:05.000000006Z"}`+"\n"+`{"kind":"random","value":"7"}`+"\n"+"not json\n"), 0o600)
	out := filepath.Join(dir, "out")
	got := runHelper(t, "CODEINSTANCES_MODE=replay", "CODEINSTANCES_VALUES_IN="+in, "CODEINSTANCES_VALUES_OUT="+out)
	ts, _ := time.Parse(time.RFC3339Nano, got[1])
	if !ts.Equal(time.Date(2020, 1, 2, 3, 4, 5, 6, time.UTC)) || got[2] != "7" {
		t.Fatal(got)
	}
	live := 0
	for _, l := range readLines(t, out) {
		if l.Live {
			live++
		}
	}
	if live != 5 {
		t.Fatalf("%d live values", live)
	}
}

func TestUnknownModeIsIgnored(t *testing.T) {
	out := runHelper(t, "CODEINSTANCES_MODE=sideways")
	if out[0] != "false" {
		t.Fatal(out)
	}
}

func TestTransportTrustsTheRunAuthority(t *testing.T) {
	auth, err := ca.New("run")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") }))
	cert, _ := auth.Issue("127.0.0.1")
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{*cert}}
	srv.StartTLS()
	defer srv.Close()

	if _, err := hook.Client().Get(srv.URL); err == nil {
		t.Fatal("trusted without the authority")
	}
	file := filepath.Join(t.TempDir(), "ca.pem")
	os.WriteFile(file, auth.CertPEM(), 0o600)
	t.Setenv(hook.EnvCA, file)
	resp, err := hook.Client().Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
}

func TestIntnPanicsOnZero(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("no panic")
		}
	}()
	hook.Intn(0)
}

func TestReplayedBytesOfAnotherLengthAreNotUsed(t *testing.T) {
	dir := t.TempDir()
	in := filepath.Join(dir, "in")
	// Eight bytes recorded; the helper asks for twelve.
	os.WriteFile(in, []byte(`{"kind":"bytes","value":"AAAAAAAAAAA="}`+"\n"), 0o600)
	out := filepath.Join(dir, "out")
	got := runHelper(t, "CODEINSTANCES_MODE=replay", "CODEINSTANCES_VALUES_IN="+in, "CODEINSTANCES_VALUES_OUT="+out)
	if !strings.HasPrefix(got[6], "12 <nil>") || strings.HasSuffix(got[6], "000000000000000000000000") {
		t.Fatal(got[6])
	}
	for _, l := range readLines(t, out) {
		if l.Kind == "bytes" && !l.Live {
			t.Fatal("a recorded value of the wrong length was used")
		}
	}
}
