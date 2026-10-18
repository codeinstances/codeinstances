// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package codeinstances

import (
	"strings"
	"testing"
	"time"

	"github.com/codeinstances/codeinstances/instance"
)

func TestNormalize(t *testing.T) {
	cases := map[string]string{
		"order 4411 failed after 2.5s":                                   "order <n> failed after <duration>",
		"took 980ms, then 1.2s":                                          "took <duration>, then <duration>",
		"at 2026-10-18T06:54:01.123456Z and 2026-10-18 07:00:00+03:00":   "at <time> and <time>",
		"Mon Oct 19 12:00:01 2026 worker 7 stopped":                      "<time> worker <n> stopped",
		"request 3f2a9c1e-77b0-4c1e-9a1b-0e6f2d8c4b11 refused":           "request <uuid> refused",
		"commit 966f00905af4 at 0xc000123abc":                            "commit <hex> at <hex>",
		"span_4bf92f3577b34da6 and commit 966f009 and ch_3NvXk2PqLr8sTu": "span_<hex> and commit <hex> and ch_<id>",
		"deadbeef stays a word, so do sha256 and utf8":                   "deadbeef stays a word, so do sha256 and utf8",
		"error: status 404 from upstream":                                "error: status 404 from upstream",
		"curl: (22) The requested URL returned error: 503":               "curl: (<n>) The requested URL returned error: 503",
		"  trims spaces  ":                                               "trims spaces",
		"index out of range [5] with length 3":                           "index out of range [<n>] with length <n>",
	}
	for in, want := range cases {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
	urls := map[string]string{
		"https://api.test/items/42?page=2&sort=asc":                 "https://api.test/items/<n>?page=*&sort=*",
		"http://127.0.0.1:41234/a/b":                                "http://127.0.0.1:<port>/a/b",
		"https://user:secret@api.test/x#frag":                       "https://api.test/x",
		"https://api.test/x?1697000000=1":                           "https://api.test/x?<n>=*",
		"https://api.test/u/3f2a9c1e-77b0-4c1e-9a1b-0e6f2d8c4b11/x": "https://api.test/u/<uuid>/x",
	}
	for in, want := range urls {
		if got := normalizeURL(in); got != want {
			t.Errorf("normalizeURL(%q) = %q, want %q", in, got, want)
		}
	}
}

const goPanic = `starting 12 workers
panic: runtime error: index out of range [7] with length 3

goroutine 18 [running]:
main.pick(...)
	/home/me/app/main.go:41
main.(*Store).Load(0xc000012345, {0x5f3a20, 0x9})
	/home/me/app/store.go:88 +0x1d4
main.main()
	/home/me/app/main.go:17 +0x65
exit status 2
`

const goTestOut = `=== RUN   TestTotals
=== RUN   TestTotals/empty
    totals_test.go:14: got 3, want 0
--- FAIL: TestTotals (0.00s)
    --- FAIL: TestTotals/empty (0.00s)
FAIL
exit status 1
FAIL	example.com/app	0.004s
`

const goTestPanic = `--- FAIL: TestParse (0.00s)
panic: boom [recovered]
	panic: boom

goroutine 7 [running]:
testing.tRunner.func1.2({0x52f2a0, 0x5c8f10})
	/usr/local/go/src/testing/testing.go:1632 +0x230
testing.tRunner.func1()
	/usr/local/go/src/testing/testing.go:1635 +0x35b
panic({0x52f2a0?, 0x5c8f10?})
	/usr/local/go/src/runtime/panic.go:785 +0x132
example.com/app.parse(...)
	/src/app/parse.go:9
example.com/app.TestParse(0xc000007340?)
	/src/app/parse_test.go:6 +0x25
testing.tRunner(0xc000007340, 0x568a28)
	/usr/local/go/src/testing/testing.go:1690 +0xf4

goroutine 1 [chan receive]:
main.other()
	/src/app/x.go:1
FAIL	example.com/app	0.005s
`

const goFatal = `fatal error: concurrent map writes

goroutine 18 [running]:
internal/runtime/maps.fatal({0x4b5c3d?, 0x0?})
	/usr/local/go/src/runtime/panic.go:1058 +0x18
main.main.func1()
	/tmp/p1.go:9 +0x4f
created by main.main in goroutine 1
	/tmp/p1.go:7 +0x45

goroutine 1 [runnable]:
main.main()
	/tmp/p1.go:12 +0x9a
exit status 2
`

const pyTrace = `fetching 3 pages
Traceback (most recent call last):
  File "/srv/app/run.py", line 40, in <module>
    main()
  File "/srv/app/run.py", line 33, in main
    total += price(item)
  File "/srv/app/lib/prices.py", line 12, in price
    return item["price"] * rate
KeyError: 'price'
`

const pyCustom = `Traceback (most recent call last):
  File "/a/run.py", line 3, in <module>
    stop()
main.<locals>.StopEverything: now
INFO shutting down
`

const nodeCrash = `/srv/app/app.js:3
  throw new Error('boom ' + Date.now());
  ^

Error: boom 1697000000123
    at Object.<anonymous> (/srv/app/app.js:3:9)
    at Module._compile (node:internal/modules/cjs/loader:1554:14)
    at Object..js (node:internal/modules/cjs/loader:1706:10)
    at Module.load (node:internal/modules/cjs/loader:1289:32)

Node.js v22.22.0
`

const javaCrash = `Exception in thread "main" java.lang.IllegalStateException: no rows for id 4411
	at com.example.Repo.load(Repo.java:42)
	at com.example.Main.main(Main.java:9)
Caused by: java.sql.SQLException: closed
	... 2 more
`

func TestErrorSignature(t *testing.T) {
	esc := string(rune(27))
	cases := []struct{ out, want string }{
		{goPanic, "panic: runtime error: index out of range [<n>] with length <n>\nat main.pick\nat main.(*Store).Load\nat main.main"},
		{goTestOut, "--- FAIL: TestTotals\n--- FAIL: TestTotals/empty"},
		{goTestPanic, "panic: boom\nat example.com/app.parse\nat example.com/app.TestParse"},
		{goFatal, "fatal error: concurrent map writes\nat internal/runtime/maps.fatal\nat main.main.func1"},
		{pyTrace, "KeyError: 'price'\nat price (prices.py)\nat main (run.py)\nat <module> (run.py)"},
		{pyCustom, "main.<locals>.StopEverything: now\nat <module> (run.py)"},
		{nodeCrash, "Error: boom <n>\nat Object.<anonymous> (app.js)\nat Module._compile (loader)\nat Object..js (loader)"},
		{javaCrash, "Exception in thread \"main\" java.lang.IllegalStateException: no rows for id <n>\nat com.example.Repo.load(Repo.java)\nat com.example.Main.main(Main.java)"},
		{"2026/10/18 07:00:01 open config.yaml: no such file or directory\nexit status 1\n", "<n>/<n>/<n> <time> open config.yaml: no such file or directory"},
		{"working\nerror: the order was refused: 503\n\n", "error: the order was refused: <n>"},
		{"done 3 of 9\nwrote report\n", "wrote report"},
		{esc + "[31merror:" + esc + "[0m disk full" + esc + "]0;pwned\a\n", "error: disk full"},
		{"progress 10%\rprogress 100%\rcannot write output\n", "cannot write output"},
		{"", ""},
	}
	for _, c := range cases {
		if got := errorSignature([]byte(c.out)); got != c.want {
			t.Errorf("got  %q\nwant %q", got, c.want)
		}
	}
	// Two failing go tests are two signatures.
	other := strings.ReplaceAll(goTestOut, "TestTotals", "TestOther")
	if errorSignature([]byte(other)) == errorSignature([]byte(goTestOut)) {
		t.Fatal("different failing tests share a signature")
	}
}

func failingRun(created time.Time, exit int, stderr string, calls ...instance.Call) *instance.Instance {
	return &instance.Instance{Created: created, Command: instance.Command{Argv: []string{"prog"}},
		Result: instance.Result{ExitCode: exit, Stderr: instance.NewBody([]byte(stderr))}, Calls: calls}
}

func TestFingerprintAndGroups(t *testing.T) {
	t0 := time.Date(2026, 10, 18, 6, 0, 0, 0, time.UTC)
	item := func(n string, status int) instance.Call {
		return call(1, "GET", "https://api.test/item/"+n+"?at=123", "", status, "x", 5)
	}
	runs := []NamedInstance{
		{"a", failingRun(t0.Add(2*time.Hour), 1, "1 items couldn't be read at 2026-10-18T08:00:00Z\n", item("7", 500), item("3", 200))},
		{"b", failingRun(t0, 1, "2 items couldn't be read at 2026-10-18T06:00:00Z\n", item("9", 500))},
		{"c", failingRun(t0.Add(time.Hour), 1, "the order was refused: 503\n", call(1, "POST", "https://api.test/order", "{}", 503, "", 5))},
		{"d", failingRun(t0, 0, "")},
		{"e", failingRun(t0.Add(3*time.Hour), 1, "1 items couldn't be read at 2026-10-18T09:00:00Z\n", item("7", 500), call(2, "GET", "https://api.test/item/8", "", 0, "connection refused", 1))},
	}
	fa, fb := FingerprintOf(runs[0].Instance), FingerprintOf(runs[1].Instance)
	if fa.ID != fb.ID || !strings.HasPrefix(fa.ID, FingerprintVersion+":") || len(fa.ID) != 20 {
		t.Fatalf("%+v\n%+v", fa, fb)
	}
	if fa.Summary() != "exit 1 | <n> items couldn't be read at <time> | GET https://api.test/item/<n>?at=* -> 500" {
		t.Fatal(fa.Summary())
	}
	g := GroupFailures(runs)
	if g.Failing != 4 || g.Passed != 1 || len(g.Groups) != 3 {
		t.Fatalf("%+v", g)
	}
	big := g.Groups[0]
	// Runs in the order they were recorded.
	if strings.Join(big.Runs, ",") != "b,a" || !big.First.Equal(t0) || !big.Last.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("%+v", big)
	}
	// Ties go to the group seen last.
	if g.Groups[1].Runs[0] != "e" || g.Groups[2].Runs[0] != "c" {
		t.Fatalf("%v %v", g.Groups[1].Runs, g.Groups[2].Runs)
	}
	// A run that also had a call with no answer is a different failure.
	if e := FingerprintOf(runs[4].Instance); e.ID == fa.ID || strings.Join(e.Calls, ";") != "GET https://api.test/item/<n> -> no answer;GET https://api.test/item/<n>?at=* -> 500" {
		t.Fatalf("%+v", e)
	}
	// So is a different way of ending.
	killed := failingRun(t0, -1, runs[0].Instance.Result.Stderr.Text, item("7", 500))
	killed.Result.Signal = "killed"
	if FingerprintOf(killed).ID == fa.ID {
		t.Fatal("a killed run shares a fingerprint with an exit 1")
	}
	if Failed(runs[3].Instance) || !Failed(killed) {
		t.Fatal("Failed")
	}
}

func TestGroupsWithoutTimes(t *testing.T) {
	runs := []NamedInstance{{"x", failingRun(time.Time{}, 1, "boom\n")}, {"y", failingRun(time.Time{}, 1, "boom\n")}}
	g := GroupFailures(runs)
	if len(g.Groups) != 1 || g.Groups[0].First != nil || g.Groups[0].Last != nil {
		t.Fatalf("%+v", g.Groups)
	}
}
