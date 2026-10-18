// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package codeinstances

import (
	"fmt"
	"strings"
	"time"

	"github.com/codeinstances/codeinstances/instance"
)

// BugReportOptions says what a bug report points to.
type BugReportOptions struct {
	// File is the instance file's name, for the commands in the report.
	File string
	// StoreURL, when set, is the store the instance was pushed to, so the
	// report says how to pull it.
	StoreURL string
	// TailLines is how many lines of output to show; zero means 20.
	TailLines int
}

// BugReport writes a Markdown bug report for an instance, to paste into an
// issue: what ran, how it ended, the error, the calls that failed, the
// fingerprint, and the commands that fetch and replay the run.
func BugReport(inst *instance.Instance, opts BugReportOptions) string {
	if opts.TailLines <= 0 {
		opts.TailLines = 20
	}
	if opts.File == "" {
		opts.File = instance.ShortID(inst.ID) + ".instance"
	}
	r := inst.Result
	var b strings.Builder
	failed := Failed(inst)
	var fp Fingerprint
	title := "The run passed"
	if failed {
		fp = FingerprintOf(inst)
		title = Ended(r)
		if fp.Error != "" {
			title += ": " + mdCode(clip(strings.SplitN(fp.Error, "\n", 2)[0]))
		}
	}
	fmt.Fprintf(&b, "## %s\n\n", title)
	fmt.Fprintf(&b, "- **Command:** %s\n", mdCode(ShellQuote(inst.Command.Argv)))
	fmt.Fprintf(&b, "- **Ended:** %s after %d ms\n", Ended(r), r.DurationMS)
	fmt.Fprintf(&b, "- **Recorded:** %s by %s\n", inst.Created.UTC().Format(time.RFC3339), mdCode(SafeText(inst.Tool)))
	fmt.Fprintf(&b, "- **Instance:** %s (%d calls, %d values)\n", mdCode(inst.ID), len(inst.Calls), len(inst.Values))
	if failed {
		fmt.Fprintf(&b, "- **Fingerprint:** %s\n", mdCode(fp.ID))
	}

	out := r.Stderr.MustBytes()
	name := "stderr"
	if len(strings.TrimSpace(string(out))) == 0 {
		out, name = r.Stdout.MustBytes(), "stdout"
	}
	if lines := cleanOutput(out); len(lines) > 0 && strings.Join(lines, "") != "" {
		shown := lines
		if len(shown) > opts.TailLines {
			shown = shown[len(shown)-opts.TailLines:]
		}
		fmt.Fprintf(&b, "\n### Output (the last %d lines of %s)\n\n", len(shown), name)
		b.WriteString(fenced(strings.Join(shown, "\n"), "text"))
	}

	var bad []string
	for _, c := range inst.Calls {
		switch {
		case c.Error != "":
			bad = append(bad, fmt.Sprintf("%s → no answer: %s", mdCode(c.Request.Method+" "+c.Request.URL), mdCode(clip(SafeText(c.Error)))))
		case c.Response.Status >= 400:
			bad = append(bad, fmt.Sprintf("%s → %d", mdCode(c.Request.Method+" "+c.Request.URL), c.Response.Status))
		}
	}
	if len(bad) > 0 {
		b.WriteString("\n### Calls that failed\n\n")
		for i, l := range bad {
			if i == 20 {
				fmt.Fprintf(&b, "- and %d more\n", len(bad)-20)
				break
			}
			fmt.Fprintf(&b, "- %s\n", l)
		}
	}

	b.WriteString("\n### Reproduce\n\n")
	file := ShellQuote([]string{opts.File})
	var cmds []string
	if opts.StoreURL != "" {
		cmds = append(cmds, fmt.Sprintf("codeinstances pull -from %s -o %s %s", ShellQuote([]string{opts.StoreURL}), file, inst.ID))
	}
	cmds = append(cmds,
		"codeinstances verify "+file,
		"codeinstances show "+file+"    # check the command it will run",
		"codeinstances replay "+file+"  # or: codeinstances replay "+file+" -- ./your-build")
	b.WriteString(fenced(strings.Join(cmds, "\n"), "sh"))
	b.WriteString("\nThe replay runs offline against the recorded answers. The instance keeps request and response bodies, URL query strings and the command line as they were; only secret header values were redacted.\n")
	return b.String()
}

// fenced puts text in a Markdown code block whose fence is longer than any
// run of backticks in it.
func fenced(text, lang string) string {
	fence := "```"
	for strings.Contains(text, fence) {
		fence += "`"
	}
	return fence + lang + "\n" + text + "\n" + fence + "\n"
}

// mdCode is inline code that holds any text.
func mdCode(s string) string {
	s = SafeText(s)
	ticks := "`"
	for strings.Contains(s, ticks) {
		ticks += "`"
	}
	if strings.HasPrefix(s, "`") || strings.HasSuffix(s, "`") {
		s = " " + s + " "
	}
	return ticks + s + ticks
}
