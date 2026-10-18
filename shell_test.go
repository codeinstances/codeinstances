// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package codeinstances

import (
	"os/exec"
	"strings"
	"testing"
)

func TestShellQuote(t *testing.T) {
	args := []string{"./sync", "--all", "two words", "a$(id)b", "it's", "`x`", "", "ünï", "tab\there", "-x=1,2"}
	q := ShellQuote(args)
	if !strings.HasPrefix(q, "./sync --all 'two words' 'a$(id)b' 'it'\\''s'") || !strings.HasSuffix(q, " -x=1,2") {
		t.Fatal(q)
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh")
	}
	// The shell reads back exactly the same arguments.
	got, err := exec.Command(sh, "-c", "set -- "+q+`; for a in "$@"; do printf '[%s]\n' "$a"; done`).Output()
	if err != nil {
		t.Fatal(err)
	}
	var want strings.Builder
	for _, a := range args {
		want.WriteString("[" + a + "]\n")
	}
	if string(got) != want.String() {
		t.Fatalf("%q\n%q", got, want.String())
	}
}

func TestSafeText(t *testing.T) {
	esc := string(rune(27))
	if got := SafeText("ok" + esc + "[31m red" + esc + "]0;title\a\nnext\r line\x00"); got != "ok red next line" {
		t.Fatalf("%q", got)
	}
}
