// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package codeinstances

import (
	"strings"
	"testing"
	"time"

	"github.com/codeinstances/codeinstances/instance"
)

func TestBugReport(t *testing.T) {
	inst := failingRun(time.Date(2026, 10, 18, 6, 0, 0, 0, time.UTC), 1,
		"loading\nnote: ```fenced``` text\nerror: request 4411 timed out after 3002ms\n",
		call(1, "GET", "https://api.test/items?page=2", "", 0, "connection refused", 3),
		call(2, "POST", "https://api.test/orders", "{}", 503, "busy", 5),
		call(3, "GET", "https://api.test/ok", "", 200, "fine", 5))
	inst.Command.Argv = []string{"./sync", "--all", "two words"}
	inst.Tool = "codeinstances 0.5.0"
	data, err := instance.Marshal(inst)
	if err != nil {
		t.Fatal(err)
	}
	inst, _ = instance.Parse(data)
	r := BugReport(inst, BugReportOptions{File: "bug 1.instance", StoreURL: "http://127.0.0.1:7979"})
	for _, want := range []string{
		"## exit 1: `error: request <n> timed out after <duration>`\n",
		"- **Command:** `./sync --all 'two words'`\n",
		"- **Ended:** exit 1 after 0 ms\n",
		"- **Instance:** `" + inst.ID + "` (3 calls, 0 values)\n",
		"- **Fingerprint:** `fp1:",
		"### Output (the last 3 lines of stderr)\n\n````text\nloading\nnote: ```fenced``` text\n",
		"- `GET https://api.test/items?page=2` → no answer: `connection refused`\n",
		"- `POST https://api.test/orders` → 503\n",
		"codeinstances pull -from http://127.0.0.1:7979 -o 'bug 1.instance' " + inst.ID + "\n",
		"codeinstances replay 'bug 1.instance'",
		"only secret header values were redacted",
	} {
		if !strings.Contains(r, want) {
			t.Errorf("report lacks %q:\n%s", want, r)
		}
	}
	if strings.Contains(r, "api.test/ok") {
		t.Fatal("a call that worked is listed")
	}
	passing := &instance.Instance{ID: "sha256:" + strings.Repeat("ab", 32), Command: instance.Command{Argv: []string{"x"}}}
	p := BugReport(passing, BugReportOptions{})
	if !strings.HasPrefix(p, "## The run passed\n") || strings.Contains(p, "Fingerprint") || strings.Contains(p, "pull") || !strings.Contains(p, "abababababab.instance") {
		t.Fatal(p)
	}
	if mdCode("x`y") != "``x`y``" || mdCode("@team <b>") != "`@team <b>`" {
		t.Fatal(mdCode("x`y"))
	}
	// A crafted command and file name are quoted so pasting them runs nothing.
	inst.Command.Argv = []string{"prog", "$(touch /tmp/x)"}
	r = BugReport(inst, BugReportOptions{File: "a$(id).instance"})
	if !strings.Contains(r, "`prog '$(touch /tmp/x)'`") || !strings.Contains(r, "codeinstances verify 'a$(id).instance'") {
		t.Fatal(r)
	}
}
