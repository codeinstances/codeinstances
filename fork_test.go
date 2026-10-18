// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package codeinstances

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codeinstances/codeinstances/instance"
	"github.com/codeinstances/codeinstances/internal/capture"
)

// sample is a small valid instance with two calls.
func sample() *instance.Instance {
	return &instance.Instance{Command: instance.Command{Argv: []string{"prog"}}, Calls: []instance.Call{
		call(1, "GET", "https://a.test/1", "", 200, "x", 1), call(2, "GET", "https://a.test/2", "", 200, "y", 1)}}
}

func forkAs(t *testing.T, w *world, parent *instance.Instance, opts ForkOptions) *instance.Instance {
	t.Helper()
	t.Setenv("CI_TEST_CHILD", "web")
	opts.Resolve = map[string]string{"secure.test": w.tls.Listener.Addr().String(), "plain.test": w.plain.Listener.Addr().String()}
	opts.UpstreamCAs = [][]byte{w.caPEM}
	opts.Timeout = time.Minute
	fork, err := Fork(context.Background(), parent, opts)
	if err != nil {
		t.Fatal(err)
	}
	// Through the file and back.
	path := filepath.Join(t.TempDir(), "fork.instance")
	if err := instance.Save(path, fork); err != nil {
		t.Fatal(err)
	}
	loaded, err := instance.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return loaded
}

func TestForkRunsLiveFromTheStep(t *testing.T) {
	w := newWorld(t)
	parent := w.record(t, "web")
	parentOut := strings.Split(string(parent.Result.Stdout.MustBytes()), "\n")

	hits := w.hits.Load()
	fork := forkAs(t, w, parent, ForkOptions{At: 2})
	if fork.Fork == nil || fork.Fork.Parent != parent.ID || fork.Fork.At != 2 {
		t.Fatalf("%+v", fork.Fork)
	}
	if len(fork.Calls) != 2 || fork.Calls[0].FromSeq != 1 || fork.Calls[1].FromSeq != 0 || fork.Calls[1].Given {
		t.Fatalf("%+v", fork.Calls)
	}
	if w.hits.Load() != hits+1 {
		t.Fatalf("%d requests went out, want 1", w.hits.Load()-hits)
	}
	out := strings.Split(string(fork.Result.Stdout.MustBytes()), "\n")
	// The hooked values and the first answer come from the parent; the
	// second answer is new.
	if out[0] != parentOut[0] || out[1] != parentOut[1] || out[2] == parentOut[2] {
		t.Fatalf("%q\n%q", out, parentOut)
	}
	if s := Summarize(fork); s != (ForkSummary{FromParent: 1, Sent: 1}) {
		t.Fatalf("%+v", s)
	}

	// Forked one past the last call, nothing goes out and the run comes out
	// as the parent did.
	hits = w.hits.Load()
	all := forkAs(t, w, parent, ForkOptions{At: 3})
	if w.hits.Load() != hits || string(all.Result.Stdout.MustBytes()) != string(parent.Result.Stdout.MustBytes()) {
		t.Fatal("a fork past the last call differs")
	}
	if c := Compare(parent, all, CompareOptions{Values: true}); !c.Same {
		t.Fatal(c.Differences)
	}
}

func TestForkOfflineWithAGivenAnswer(t *testing.T) {
	w := newWorld(t)
	parent := w.record(t, "web")
	w.down()
	fork := forkAs(t, w, parent, ForkOptions{At: 1, Offline: true, Answer: &Answer{Status: 503, Body: "busy"}})
	if len(fork.Calls) != 2 || !fork.Calls[0].Given || fork.Calls[0].Response.Status != 503 || fork.Calls[1].Error != capture.ErrOffline {
		t.Fatalf("%+v", fork.Calls)
	}
	if !strings.Contains(string(fork.Result.Stdout.MustBytes()), "503 busy") {
		t.Fatal(string(fork.Result.Stdout.MustBytes()))
	}
	if s := Summarize(fork); s != (ForkSummary{Given: 1, Offline: 1}) {
		t.Fatalf("%+v", s)
	}
	// The fork replays like any instance.
	if rep := replayAs(t, fork, "web", ReplayOptions{}); !rep.Matched {
		t.Fatal(rep.Divergences)
	}
	// And diff shows what the given answer changed.
	c := Compare(parent, fork, CompareOptions{})
	if c.Same || !strings.Contains(strings.Join(c.Differences, "\n"), "status: a 200, b 503") {
		t.Fatal(c.Differences)
	}
}

func TestForkValuesAndErrors(t *testing.T) {
	w := newWorld(t)
	parent := w.record(t, "web")
	fresh := forkAs(t, w, parent, ForkOptions{At: 3, FreshValues: true})
	if fresh.Values[0].Value == parent.Values[0].Value {
		t.Fatal("fresh values came from the parent")
	}
	t.Setenv("CI_TEST_CHILD", "web")
	for _, opts := range []ForkOptions{{At: 0}, {At: 4}, {At: 3, Answer: &Answer{Status: 200}}} {
		if _, err := Fork(context.Background(), parent, opts); err == nil {
			t.Errorf("%+v: no error", opts)
		}
	}
}

func TestReadAnswer(t *testing.T) {
	dir := t.TempDir()
	write := func(s string) string {
		p := filepath.Join(dir, "a.json")
		os.WriteFile(p, []byte(s), 0o600)
		return p
	}
	a, err := ReadAnswer(write(`{"status": 503, "header": {"Retry-After": ["30"]}, "body": "busy"}`))
	if err != nil || a.Status != 503 || a.Header["Retry-After"][0] != "30" {
		t.Fatal(a, err)
	}
	if a, err := ReadAnswer(write(`{"error": "connection refused"}`)); err != nil || a.Error == "" {
		t.Fatal(err)
	}
	for _, bad := range []string{`{}`, `{"status": 99}`, `{"status": 103}`, `{"status": 200, "error": "x"}`, `{"status": 200, "bdy": "typo"}`, `nope`,
		`{"error": "x", "body": "y"}`, `{"error": "a\u001b[2J"}`, `{"status": 200, "header": {"Content-Length": ["5"]}}`,
		`{"status": 200, "header": {"X-A": ["a\r\nInjected: 1"]}}`, `{"status": 200, "header": {"bad name": ["x"]}}`} {
		if _, err := ReadAnswer(write(bad)); err == nil {
			t.Errorf("%s: accepted", bad)
		}
	}
}

func TestForkRules(t *testing.T) {
	inst := sample()
	inst.Calls[0].FromSeq = 1
	if inst.Validate() == nil {
		t.Fatal("from_seq without a fork")
	}
	inst.Fork = &instance.Fork{Parent: "sha256:" + strings.Repeat("a", 64), At: 1}
	if inst.Validate() == nil {
		t.Fatal("from_seq at the step")
	}
	inst.Fork.At = 2
	if err := inst.Validate(); err != nil {
		t.Fatal(err)
	}
	inst.Calls[0].FromSeq, inst.Calls[0].Given, inst.Calls[1].Given = 0, true, true
	if inst.Validate() == nil {
		t.Fatal("two given answers")
	}
	inst = sample()
	inst.Command.Env = map[string]string{"no_proxy": "*"}
	if inst.Validate() == nil {
		t.Fatal("a kept proxy variable")
	}
}

func TestShrinkOfAForkIsARunOfItsOwn(t *testing.T) {
	pass := recordedResult(t, sixCalls(t, []string{"good", "good", "good", "good", "good", "good"}))
	fail := recordedResult(t, sixCalls(t, []string{"good", "good", "bad", "good", "bad", "good"}, "report"))
	fail.Fork = &instance.Fork{Parent: "sha256:" + strings.Repeat("b", 64), At: 3}
	fail.Calls[0].FromSeq, fail.Calls[1].FromSeq = 1, 2
	res, err := Shrink(context.Background(), pass, fail, ShrinkOptions{Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if res.Instance.Fork != nil || res.Instance.Calls[0].FromSeq != 0 || res.Instance.Validate() != nil {
		t.Fatalf("%+v", res.Instance.Fork)
	}
}
