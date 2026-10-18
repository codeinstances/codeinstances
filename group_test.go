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
		"order 4411 failed after 2.5s":                                 "order <n> failed after <n>s",
		"at 2026-10-18T06:54:01.123456Z and 2026-10-18 07:00:00+03:00": "at <time> and <time>",
		"request 3f2a9c1e-77b0-4c1e-9a1b-0e6f2d8c4b11 refused":         "request <uuid> refused",
		"commit 966f00905af4 at 0xc000123abc":                          "commit <hex> at <hex>",
		"deadbeef stays a word, so does sha256 mostly":                 "deadbeef stays a word, so does sha<n> mostly",
		"  trims spaces  ":                                             "trims spaces",
		"index out of range [5] with length 3":                         "index out of range [<n>] with length <n>",
	}
	for in, want := range cases {
		if got := normalize(in); got != want {
			t.Errorf("normalize(%q) = %q, want %q", in, got, want)
		}
	}
	urls := map[string]string{
		"https://api.test/items/42?page=2&sort=asc":                 "https://api.test/items/<n>?page=*&sort=*",
		"http://127.0.0.1:8080/a/b":                                 "http://127.0.0.1:8080/a/b",
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

func TestErrorSignature(t *testing.T) {
	cases := []struct{ out, want string }{
		{goPanic, "panic: runtime error: index out of range [<n>] with length <n>\nat main.pick\nat main.(*Store).Load\nat main.main"},
		{pyTrace, "KeyError: 'price'\nat price (prices.py)\nat main (run.py)\nat <module> (run.py)"},
		{"working\nerror: the order was refused: 503\n\n", "error: the order was refused: <n>"},
		{"", ""},
	}
	for _, c := range cases {
		if got := errorSignature([]byte(c.out)); got != c.want {
			t.Errorf("got %q\nwant %q", got, c.want)
		}
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
	if fa.ID != fb.ID || !strings.HasPrefix(fa.ID, "fp:") || len(fa.ID) != 19 {
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
	if strings.Join(big.Runs, ",") != "a,b" || !big.First.Equal(t0) || !big.Last.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("%+v", big)
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
