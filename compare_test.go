// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package codeinstances

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/codeinstances/codeinstances/instance"
)

func ms(n int64) *int64 { return &n }

func call(seq int, method, url, reqBody string, status int, respBody string, took int64) instance.Call {
	c := instance.Call{Seq: seq, Kind: instance.KindHTTP, AtMS: int64(seq), DurationMS: ms(took),
		Request: instance.Request{Method: method, URL: url, Body: instance.NewBody([]byte(reqBody))}}
	if status == 0 {
		c.Error = respBody
	} else {
		c.Response = &instance.Response{Status: status, Body: instance.NewBody([]byte(respBody))}
	}
	return c
}

func base() *instance.Instance {
	return &instance.Instance{Format: instance.Format, ID: "sha256:aaaa", Tool: "codeinstances " + Version,
		Command: instance.Command{Argv: []string{"prog", "-x"}},
		Result:  instance.Result{ExitCode: 0, DurationMS: 100, Stdout: instance.NewBody([]byte("one\ntwo\n"))},
		Calls: []instance.Call{
			call(1, "GET", "https://api.test/a", "", 200, `{"items":[{"price":10}],"ok":true}`, 20),
			call(2, "POST", "https://api.test/b", "q=1", 200, "fine", 30),
		},
		Values: []instance.Value{{Seq: 1, Kind: instance.ValueClock, Value: "2026-10-18T00:00:00Z"}, {Seq: 2, Kind: instance.ValueRandom, Value: "7"}},
	}
}

func has(t *testing.T, c *Comparison, want ...string) {
	t.Helper()
	all := strings.Join(c.Differences, "\n")
	for _, w := range want {
		if !strings.Contains(all, w) {
			t.Errorf("no difference mentions %q; got:\n%s", w, all)
		}
	}
}

func TestCompareSame(t *testing.T) {
	a, b := base(), base()
	b.ID = "sha256:bbbb"
	b.Values[0].Value = "2026-10-18T09:00:00Z"
	b.Values[1].Value = "8"
	c := Compare(a, b, CompareOptions{})
	if !c.Same || len(c.Differences) != 0 || c.Calls.Same != 2 {
		t.Fatalf("%+v", c)
	}
	if len(c.Speed.Calls) != 2 {
		t.Fatalf("speed %+v", c.Speed)
	}
	// With -values the values themselves count.
	c = Compare(a, b, CompareOptions{Values: true})
	has(t, c, "clock value 1: a 2026-10-18T00:00:00Z, b 2026-10-18T09:00:00Z", "random value 1: a 7, b 8")
}

func TestCompareEndingAndOutput(t *testing.T) {
	a, b := base(), base()
	b.Result.ExitCode = 1
	b.Result.Stdout = instance.NewBody([]byte("one\nTWO\n"))
	b.Result.Stderr = instance.NewBody([]byte("boom\n"))
	c := Compare(a, b, CompareOptions{})
	has(t, c, "ended: a exit 0, b exit 1", `stdout line 2: a "two", b "TWO"`, `stderr line 1: b printed "boom", which a didn't`)
	if c.Same {
		t.Fatal("same")
	}
	c = Compare(a, b, CompareOptions{IgnoreStderr: true})
	if strings.Contains(strings.Join(c.Differences, "\n"), "stderr") {
		t.Fatal("stderr compared")
	}
	b.Result = instance.Result{Signal: "killed"}
	has(t, Compare(a, b, CompareOptions{}), "ended: a exit 0, b killed by killed")
	b.Result = instance.Result{TimedOut: true, ExitCode: -1, Signal: "killed"}
	has(t, Compare(a, b, CompareOptions{}), "ended: a exit 0, b timed out")
}

func TestCompareCalls(t *testing.T) {
	a, b := base(), base()
	b.Calls = []instance.Call{
		call(1, "GET", "https://api.test/a", "", 500, `{"items":[{"price":12}],"ok":true}`, 25),
		call(2, "POST", "https://api.test/b", "q=2", 200, "fine", 30),
		call(3, "GET", "https://api.test/new", "", 200, "x", 5),
	}
	c := Compare(a, b, CompareOptions{})
	has(t, c,
		"GET https://api.test/a (a call 1, b call 1): status: a 200, b 500",
		"GET https://api.test/a (a call 1, b call 1): response body at $.items[0].price: a 10, b 12",
		`POST https://api.test/b (a call 2, b call 2): request body line 1: a "q=1", b "q=2"`,
		"GET https://api.test/new (b call 3) is only in b",
	)
	if k := c.Calls; k.Same != 0 || k.Changed != 2 || k.OnlyA != 0 || k.OnlyB != 1 {
		t.Fatalf("%+v", k)
	}

	// A missing call, and pairing that follows order for repeated requests.
	b = base()
	b.Calls = []instance.Call{call(1, "GET", "https://api.test/a", "", 200, `{"items":[{"price":10}],"ok":true}`, 20)}
	c = Compare(a, b, CompareOptions{})
	has(t, c, "POST https://api.test/b (a call 2) is only in a")
	if c.Calls.OnlyA != 1 || c.Calls.Same != 1 {
		t.Fatalf("%+v", c.Calls)
	}
}

func TestCompareErrorsAndBodies(t *testing.T) {
	a, b := base(), base()
	b.Calls[0] = call(1, "GET", "https://api.test/a", "", 0, "connection refused", 3)
	has(t, Compare(a, b, CompareOptions{}), "a got status 200, b got no answer (connection refused)")
	has(t, Compare(b, a, CompareOptions{}), "a got no answer (connection refused), b got status 200")

	cases := []struct{ a, b, want string }{
		{`{"x":1}`, `{"x":1,"y":2}`, "at $.y: only in b (2)"},
		{`{"x":1,"y":2}`, `{"x":1}`, "at $.y: only in a (2)"},
		{`[1,2]`, `[1,2,3]`, "at $: 2 items in a, 3 in b"},
		{`{"x":"1"}`, `{"x":1}`, `at $.x: a "1", b 1`},
		{`{"a b":true}`, `{"a b":false}`, `at $["a b"]: a true, b false`},
		{`{"x":1, "y":2}`, `{"y":2,"x":1}`, "is the same JSON written differently"},
		{`{"x":1.0}`, `{"x":1}`, "at $.x: a 1.0, b 1"},
		{"\xff\x00", "\xfe", "differs from byte 1 (2 bytes in a, 1 in b)"},
		{"\xff\x00\x01", "\xff\x00\x02", "differs from byte 3 (3 bytes in a, 3 in b)"},
		{"", "hello", `line 1 is only in b: "hello"`},
		{"a\nb", "a\nb\n", "differs only in its last newline"},
		{"a\n", "a\nb\n", `line 2 is only in b: "b"`},
	}
	for _, tc := range cases {
		got := bodyDiff("body", []byte(tc.a), []byte(tc.b))
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s vs %s: %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
	if bodyDiff("body", []byte("same"), []byte("same")) != "" {
		t.Fatal("equal bodies differ")
	}
}

func TestCompareValueCounts(t *testing.T) {
	a, b := base(), base()
	b.Values = append(b.Values, instance.Value{Seq: 3, Kind: instance.ValueRandom, Value: "9"})
	has(t, Compare(a, b, CompareOptions{}), "random values: a asked for 1, b for 2")
}

func TestCompareNotesDontCount(t *testing.T) {
	a, b := base(), base()
	b.Command.Argv = []string{"prog-fixed", "-x"}
	b.Command.Env = map[string]string{"MODE": "fast"}
	b.Tool = "codeinstances 0.0.9"
	c := Compare(a, b, CompareOptions{})
	if !c.Same || len(c.Notes) != 3 {
		t.Fatalf("%+v", c)
	}
	notes := strings.Join(c.Notes, "\n")
	for _, w := range []string{"a ran prog -x, b ran prog-fixed -x", "kept environment differs in MODE", "a by codeinstances " + Version + ", b by codeinstances 0.0.9"} {
		if !strings.Contains(notes, w) {
			t.Errorf("notes lack %q: %s", w, notes)
		}
	}
}

func TestCompareSpeedOrder(t *testing.T) {
	a, b := base(), base()
	b.Calls[0].DurationMS = ms(21)  // +1
	b.Calls[1].DurationMS = ms(330) // +300
	b.Result.DurationMS = 410
	c := Compare(a, b, CompareOptions{})
	if !c.Same {
		t.Fatal(c.Differences)
	}
	s := c.Speed
	if s.ADurationMS != 100 || s.BDurationMS != 410 || len(s.Calls) != 2 || s.Calls[0].URL != "https://api.test/b" || s.Calls[0].BMS != 330 {
		t.Fatalf("%+v", s)
	}
	// Calls recorded before 0.2.0 have no time and are left out.
	b.Calls[0].DurationMS = nil
	if c := Compare(a, b, CompareOptions{}); len(c.Speed.Calls) != 1 {
		t.Fatalf("%+v", c.Speed)
	}
}

func TestCompareRecordedRuns(t *testing.T) {
	w := newWorld(t)
	pass := w.record(t, "web")
	for _, c := range pass.Calls {
		if c.DurationMS == nil || *c.DurationMS < 0 {
			t.Fatalf("call %d has no duration", c.Seq)
		}
	}
	if c := Compare(pass, pass, CompareOptions{Values: true}); !c.Same || c.Calls.Same != 2 || len(c.Speed.Calls) != 2 {
		t.Fatalf("an instance differs from itself: %+v", c)
	}
	t.Setenv("CHILD_EXIT", "3")
	fail := w.record(t, "extra")
	c := Compare(pass, fail, CompareOptions{})
	if c.Same {
		t.Fatal("same")
	}
	// The servers answer with a counter, so the paired responses differ too.
	has(t, c, "ended: a exit 0, b exit 3", "GET https://secure.test/extra (b call 3) is only in b",
		"GET https://secure.test/a (a call 1, b call 1): response body line 1")
	if c.Calls.OnlyB != 1 || c.Calls.Changed != 2 {
		t.Fatalf("%+v", c.Calls)
	}
}

func TestClipKeepsCharactersWhole(t *testing.T) {
	s := strings.Repeat("x", 116) + strings.Repeat("é", 10)
	got := clip(s)
	if !utf8.ValidString(got) || !strings.HasSuffix(got, "...") || len(got) > 120 {
		t.Fatalf("%q", got)
	}
}
