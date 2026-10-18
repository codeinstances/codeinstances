// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

// Command codeinstances records runs of programs as instances and replays
// them.
//
//	codeinstances record [flags] -- command [args...]
//	codeinstances replay [flags] file [-- command [args...]]
//	codeinstances diff [flags] a.instance b.instance
//	codeinstances shrink [flags] -pass passing.instance failing.instance [-- command [args...]]
//	codeinstances test [flags] file-or-dir... [-- command [args...]]
//	codeinstances group [flags] file-or-dir...
//	codeinstances fork [flags] -at N parent.instance [-- command [args...]]
//	codeinstances report [flags] file
//	codeinstances serve [flags]
//	codeinstances push [flags] file...
//	codeinstances pull [flags] id
//	codeinstances show [-json] file
//	codeinstances verify file
//	codeinstances version [-json]
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/codeinstances/codeinstances"
	"github.com/codeinstances/codeinstances/instance"
	"github.com/codeinstances/codeinstances/internal/buildinfo"
	"github.com/codeinstances/codeinstances/store"
)

const usage = `CodeInstances keeps runs of programs as instances and replays them.

Usage:
  codeinstances record [flags] -- command [args...]
      Run the command and keep the run as an instance file: its output, how
      it ended, every HTTP(S) exchange and every hooked clock or random value.
  codeinstances replay [flags] file [-- command [args...]]
      Run the recorded command again (or the one given) with the network
      replaced by the recording, and report whether it came out the same.
  codeinstances diff [flags] a.instance b.instance
      Compare two runs, such as a passing one and a failing one: how they
      ended, what they printed, their calls and values, and their speed.
  codeinstances shrink [flags] -pass passing.instance failing.instance [-- command]
      Find the fewest of the failing run's answers that still make the
      program fail the same way, and keep that smallest failing instance.
  codeinstances test [flags] file-or-dir... [-- command [args...]]
      Replay instances as regression tests: a run that passed must replay
      exactly, and a run that failed must now exit 0.
  codeinstances group [flags] file-or-dir...
      Fingerprint failing runs and put the ones that fail the same way
      together, so repeats of one bug show as one group.
  codeinstances fork [flags] -at N parent.instance [-- command [args...]]
      Run the parent's program again with the parent's answers for the calls
      before step N, and live from there on, and keep that run as a fork.
  codeinstances report [-url store] file
      Write a Markdown bug report for an instance, to paste into an issue.
  codeinstances serve [flags]
      Run a store: an HTTP server that keeps instances under their ids.
  codeinstances push [-to url] file...
      Upload instances to a store and print their ids and links.
  codeinstances pull [-from url] [-o file] id
      Download an instance from a store by id or unique prefix.
  codeinstances show [-json] file
      Describe an instance.
  codeinstances verify file
      Check that an instance file hasn't been changed since it was recorded.
  codeinstances version [-json]

Run "codeinstances COMMAND -h" for a command's flags.
Exit status: record and fork exit as the program did; replay exits 0 when the run
matched, 1 when it diverged; diff exits 0 when the runs are the same, 1 when
they differ; shrink exits 0 when it kept an instance, 1 when the failure
couldn't be narrowed down; test exits 0 when every test passed, 1
when any failed; group, report, push and pull exit 0 when they worked; any
command exits 2 on a usage, file or network error. push, pull and report
read the store's address from CODEINSTANCES_STORE and push, pull and serve
the token from CODEINSTANCES_TOKEN when no flag gives them.
`

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 2
	}
	switch args[0] {
	case "record":
		return record(args[1:], stdout, stderr)
	case "replay":
		return replay(args[1:], stdout, stderr)
	case "diff":
		return diff(args[1:], stdout, stderr)
	case "shrink":
		return shrink(args[1:], stdout, stderr)
	case "test":
		return test(args[1:], stdout, stderr)
	case "group":
		return group(args[1:], stdout, stderr)
	case "report":
		return report(args[1:], stdout, stderr)
	case "fork":
		return fork(args[1:], stdout, stderr)
	case "serve":
		return serve(args[1:], stdout, stderr)
	case "push":
		return push(args[1:], stdout, stderr)
	case "pull":
		return pull(args[1:], stdout, stderr)
	case "show":
		return show(args[1:], stdout, stderr)
	case "verify":
		return verify(args[1:], stdout, stderr)
	case "version", "-version", "--version":
		return version(args[1:], stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return 0
	}
	fmt.Fprintf(stderr, "codeinstances: unknown command %q\n\n%s", args[0], usage)
	return 2
}

type list []string

func (l *list) String() string     { return strings.Join(*l, ",") }
func (l *list) Set(v string) error { *l = append(*l, v); return nil }

// splitCommand separates flags from the command after "--".
func splitCommand(args []string) (flags, command []string) {
	for i, a := range args {
		if a == "--" {
			return args[:i], args[i+1:]
		}
	}
	return args, nil
}

func record(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("record", flag.ContinueOnError)
	fs.SetOutput(stderr)
	out := fs.String("o", "run.instance", "the instance file to write")
	var env, resolve, cas list
	fs.Var(&env, "env", "keep this environment variable in the instance and set it on replay (repeatable)")
	fs.Var(&resolve, "resolve", "send host[:port] to another address, as host=addr[:port] (repeatable)")
	fs.Var(&cas, "upstream-ca", "also trust this PEM file's certificates when reaching real HTTPS servers (repeatable)")
	keep := fs.Bool("keep-secrets", false, "keep Authorization, Cookie and similar headers instead of redacting them")
	timeout := fs.Duration("timeout", 0, "kill the program after this long (0: no limit)")
	quiet := fs.Bool("q", false, "don't pass the program's output through")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: codeinstances record [flags] -- command [args...]")
		fs.PrintDefaults()
	}
	flagArgs, command := splitCommand(args)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	if fs.NArg() > 0 && command == nil {
		command = fs.Args()
	} else if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "codeinstances record: unexpected %q before --\n", fs.Arg(0))
		return 2
	}
	if len(command) == 0 {
		fs.Usage()
		return 2
	}
	opts := codeinstances.RecordOptions{Argv: command, KeepEnv: env, KeepSecrets: *keep, Timeout: *timeout, Resolve: map[string]string{}}
	for _, r := range resolve {
		host, addr, ok := strings.Cut(r, "=")
		if !ok || host == "" || addr == "" {
			fmt.Fprintf(stderr, "codeinstances record: -resolve %q: want host=addr\n", r)
			return 2
		}
		opts.Resolve[host] = addr
	}
	for _, path := range cas {
		pem, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(stderr, "codeinstances record: %v\n", err)
			return 2
		}
		opts.UpstreamCAs = append(opts.UpstreamCAs, pem)
	}
	if !*quiet {
		opts.Stdout, opts.Stderr = stdout, stderr
	}
	ctx, stop := interrupts()
	defer stop()
	inst, err := codeinstances.Record(ctx, opts)
	if err != nil {
		fmt.Fprintf(stderr, "codeinstances record: %v\n", err)
		return 2
	}
	if err := instance.Save(*out, inst); err != nil {
		fmt.Fprintf(stderr, "codeinstances record: %v\n", err)
		return 2
	}
	fmt.Fprintf(stderr, "codeinstances: kept %s (%s)\n", *out, inst.Summary())
	switch {
	case inst.Result.ExitCode >= 0:
		return inst.Result.ExitCode
	default:
		return 1
	}
}

func replay(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("replay", flag.ContinueOnError)
	fs.SetOutput(stderr)
	strict := fs.Bool("strict", false, "require HTTP requests in exactly the recorded order")
	noStderr := fs.Bool("ignore-stderr", false, "leave stderr out of the comparison")
	asJSON := fs.Bool("json", false, "print the report as JSON")
	timeout := fs.Duration("timeout", 0, "kill the program after this long (0: no limit)")
	quiet := fs.Bool("q", false, "don't pass the program's output through")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: codeinstances replay [flags] file [-- command [args...]]")
		fs.PrintDefaults()
	}
	flagArgs, command := splitCommand(args)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	inst, err := instance.Load(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "codeinstances replay: %v\n", err)
		return 2
	}
	opts := codeinstances.ReplayOptions{Argv: command, Strict: *strict, IgnoreStderr: *noStderr, Timeout: *timeout}
	if !*quiet && !*asJSON {
		opts.Stdout, opts.Stderr = stdout, stderr
	}
	ctx, stop := interrupts()
	defer stop()
	rep, err := codeinstances.Replay(ctx, inst, opts)
	if err != nil {
		fmt.Fprintf(stderr, "codeinstances replay: %v\n", err)
		return 2
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(rep)
	} else {
		printReport(stderr, rep)
	}
	if rep.Matched {
		return 0
	}
	return 1
}

func printReport(w io.Writer, rep *codeinstances.Report) {
	if rep.Matched {
		fmt.Fprintf(w, "codeinstances: replay matched (%d of %d calls served, %d of %d values used)\n", rep.CallsServed, rep.Calls, rep.ValuesUsed, rep.Values)
		return
	}
	fmt.Fprintf(w, "codeinstances: replay diverged in %d ways:\n", len(rep.Divergences))
	for _, d := range rep.Divergences {
		fmt.Fprintf(w, "  - %s\n", d)
	}
}

// maxListed is how many differences diff prints before summing up the rest.
const maxListed = 50

func diff(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	fs.SetOutput(stderr)
	noStderr := fs.Bool("ignore-stderr", false, "leave stderr out of the comparison")
	values := fs.Bool("values", false, "compare the hooked values (clock, random, bytes) themselves, not only how many of each kind")
	asJSON := fs.Bool("json", false, "print the comparison as JSON")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: codeinstances diff [flags] a.instance b.instance (flags go before the files)")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 2 {
		fs.Usage()
		return 2
	}
	var insts [2]*instance.Instance
	for i := range insts {
		inst, err := instance.Load(fs.Arg(i))
		if err != nil {
			fmt.Fprintf(stderr, "codeinstances diff: %v\n", err)
			return 2
		}
		insts[i] = inst
	}
	c := codeinstances.Compare(insts[0], insts[1], codeinstances.CompareOptions{IgnoreStderr: *noStderr, Values: *values})
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(c)
	} else {
		printComparison(stdout, fs.Arg(0), fs.Arg(1), c)
	}
	if c.Same {
		return 0
	}
	return 1
}

func printComparison(w io.Writer, nameA, nameB string, c *codeinstances.Comparison) {
	for _, s := range []struct {
		label, name string
		side        codeinstances.Side
	}{{"a", nameA, c.A}, {"b", nameB, c.B}} {
		fmt.Fprintf(w, "%s  %s  %s, recorded %s\n", s.label, s.name, instance.ShortID(s.side.ID), s.side.Created.UTC().Format("2006-01-02 15:04 UTC"))
		fmt.Fprintf(w, "   %s after %d ms, %d calls, %d values\n", s.side.Ended, s.side.DurationMS, s.side.Calls, s.side.Values)
	}
	k := c.Calls
	fmt.Fprintf(w, "calls  %d the same, %d changed, %d only in a, %d only in b\n", k.Same, k.Changed, k.OnlyA, k.OnlyB)
	for _, n := range c.Notes {
		fmt.Fprintf(w, "note   %s\n", n)
	}
	sp := c.Speed
	fmt.Fprintf(w, "speed  a %d ms, b %d ms (%s)\n", sp.ADurationMS, sp.BDurationMS, change(sp.ADurationMS, sp.BDurationMS))
	shown := 0
	for _, cs := range sp.Calls {
		if shown == 5 || cs.AMS == cs.BMS {
			break
		}
		if shown == 0 {
			fmt.Fprintln(w, "       calls whose time changed most:")
		}
		fmt.Fprintf(w, "       %+6d ms  %s %s (a call %d: %d ms, b call %d: %d ms)\n", cs.BMS-cs.AMS, cs.Method, cs.URL, cs.ASeq, cs.AMS, cs.BSeq, cs.BMS)
		shown++
	}
	if c.Same {
		fmt.Fprintln(w, "codeinstances: the runs are the same")
		return
	}
	way := "ways"
	if len(c.Differences) == 1 {
		way = "way"
	}
	fmt.Fprintf(w, "codeinstances: the runs differ in %d %s:\n", len(c.Differences), way)
	for i, d := range c.Differences {
		if i == maxListed {
			fmt.Fprintf(w, "  ... and %d more (diff -json lists them all)\n", len(c.Differences)-maxListed)
			break
		}
		fmt.Fprintf(w, "  - %s\n", d)
	}
}

// change describes how long b took next to a.
func change(a, b int64) string {
	switch {
	case a == b:
		return "no change"
	case a > 0:
		return fmt.Sprintf("%+d ms, %.2fx", b-a, float64(b)/float64(a))
	}
	return fmt.Sprintf("%+d ms", b-a)
}

func show(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the whole instance as JSON")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(stderr, "usage: codeinstances show [-json] file")
		return 2
	}
	data, err := os.ReadFile(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "codeinstances show: %v\n", err)
		return 2
	}
	inst, err := instance.Parse(data)
	if err != nil {
		fmt.Fprintf(stderr, "codeinstances show: %s: %v\n", fs.Arg(0), err)
		return 2
	}
	if *asJSON {
		// The file as it is, so its id still verifies.
		stdout.Write(data)
		return 0
	}
	r := inst.Result
	fmt.Fprintf(stdout, "instance  %s\n", inst.ID)
	fmt.Fprintf(stdout, "recorded  %s by %s\n", inst.Created.Format(time.RFC3339), codeinstances.SafeText(inst.Tool))
	fmt.Fprintf(stdout, "command   %s\n", codeinstances.SafeText(codeinstances.ShellQuote(inst.Command.Argv)))
	if f := inst.Fork; f != nil {
		fmt.Fprintf(stdout, "forked    from %s at call %d\n", f.Parent, f.At)
	}
	switch {
	case r.TimedOut:
		fmt.Fprintf(stdout, "ended     timed out after %d ms\n", r.DurationMS)
	case r.Signal != "":
		fmt.Fprintf(stdout, "ended     killed by %s after %d ms\n", codeinstances.SafeText(r.Signal), r.DurationMS)
	default:
		fmt.Fprintf(stdout, "ended     exit %d after %d ms\n", r.ExitCode, r.DurationMS)
	}
	fmt.Fprintf(stdout, "output    %d bytes on stdout, %d on stderr\n", len(r.Stdout.MustBytes()), len(r.Stderr.MustBytes()))
	fmt.Fprintf(stdout, "calls     %d\n", len(inst.Calls))
	for _, c := range inst.Calls {
		took := ""
		if c.DurationMS != nil {
			took = fmt.Sprintf(" in %d ms", *c.DurationMS)
		}
		switch {
		case c.FromSeq > 0:
			took += fmt.Sprintf(", answered from the parent's call %d", c.FromSeq)
		case c.Given:
			took += ", answer given at the fork"
		}
		if c.Error != "" {
			fmt.Fprintf(stdout, "  %3d  %6d ms  %s %s -> error%s: %s\n", c.Seq, c.AtMS, codeinstances.SafeText(c.Request.Method), codeinstances.SafeText(c.Request.URL), took, codeinstances.SafeText(c.Error))
		} else {
			fmt.Fprintf(stdout, "  %3d  %6d ms  %s %s -> %d (%d bytes)%s\n", c.Seq, c.AtMS, codeinstances.SafeText(c.Request.Method), codeinstances.SafeText(c.Request.URL), c.Response.Status, len(c.Response.Body.MustBytes()), took)
		}
	}
	counts := map[string]int{}
	for _, v := range inst.Values {
		counts[v.Kind]++
	}
	fmt.Fprintf(stdout, "values    %d (clock %d, random %d, bytes %d)\n", len(inst.Values), counts[instance.ValueClock], counts[instance.ValueRandom], counts[instance.ValueBytes])
	if codeinstances.Failed(inst) {
		fp := codeinstances.FingerprintOf(inst)
		fmt.Fprintf(stdout, "failure   %s  %s\n", fp.ID, codeinstances.SafeText(fp.Summary()))
	}
	return 0
}

func verify(args []string, stdout, stderr io.Writer) int {
	if len(args) != 1 {
		fmt.Fprintln(stderr, "usage: codeinstances verify file")
		return 2
	}
	data, err := os.ReadFile(args[0])
	if err != nil {
		fmt.Fprintf(stderr, "codeinstances verify: %v\n", err)
		return 2
	}
	if _, err := instance.Parse(data); err != nil {
		fmt.Fprintf(stderr, "codeinstances verify: %s: %v\n", args[0], err)
		if errors.Is(err, instance.ErrModified) {
			return 1
		}
		return 2
	}
	var head struct {
		ID string `json:"id"`
	}
	json.Unmarshal(data, &head)
	fmt.Fprintf(stdout, "%s: intact (%s)\n", args[0], head.ID)
	return 0
}

func version(args []string, stdout, stderr io.Writer) int {
	info := buildinfo.Info()
	if len(args) == 1 && (args[0] == "-json" || args[0] == "--json") {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(info)
		return 0
	}
	if len(args) > 0 {
		fmt.Fprintln(stderr, "usage: codeinstances version [-json]")
		return 2
	}
	mod := ""
	if info.Modified {
		mod = " (modified)"
	}
	fmt.Fprintf(stdout, "codeinstances %s (format %s, commit %s%s, %s %s/%s)\n", info.Version, info.Format, info.Revision, mod, info.Go, info.OS, info.Arch)
	return 0
}

// interrupts lets Ctrl-C reach the program (it shares the terminal) while
// codeinstances keeps running to write what happened.
func interrupts() (context.Context, func()) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	return context.Background(), func() { signal.Stop(ch) }
}

func shrink(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("shrink", flag.ContinueOnError)
	fs.SetOutput(stderr)
	passFile := fs.String("pass", "", "the passing run to take answers from (required)")
	out := fs.String("o", "shrunk.instance", "the instance file to write")
	match := fs.String("match", "", "a regular expression the program's output must also match to count as the same failure")
	maxRuns := fs.Int("max-runs", 200, "run the program at most this many times")
	timeout := fs.Duration("timeout", time.Minute, "kill the program after this long, in each run (0: no limit)")
	quiet := fs.Bool("q", false, "don't report each run")
	asJSON := fs.Bool("json", false, "print the result as JSON")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: codeinstances shrink [flags] -pass passing.instance failing.instance [-- command [args...]]")
		fs.PrintDefaults()
	}
	flagArgs, command := splitCommand(args)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	if fs.NArg() != 1 || *passFile == "" {
		fs.Usage()
		return 2
	}
	opts := codeinstances.ShrinkOptions{Argv: command, Timeout: *timeout, MaxRuns: *maxRuns}
	if *match != "" {
		re, err := regexp.Compile(*match)
		if err != nil {
			fmt.Fprintf(stderr, "codeinstances shrink: -match: %v\n", err)
			return 2
		}
		opts.Match = re
	}
	if !*quiet {
		opts.Progress = stderr
	}
	pass, err := instance.Load(*passFile)
	if err != nil {
		fmt.Fprintf(stderr, "codeinstances shrink: %v\n", err)
		return 2
	}
	fail, err := instance.Load(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "codeinstances shrink: %v\n", err)
		return 2
	}
	// Ctrl-C reaches the program and stops the search: an interrupted run
	// says nothing about the answers.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	res, err := codeinstances.Shrink(ctx, pass, fail, opts)
	if err != nil {
		fmt.Fprintf(stderr, "codeinstances shrink: %v\n", err)
		if errors.Is(err, codeinstances.ErrNotAFailure) || errors.Is(err, codeinstances.ErrFailureDoesNotReplay) || errors.Is(err, codeinstances.ErrNotTheAnswers) {
			return 1
		}
		return 2
	}
	if err := instance.Save(*out, res.Instance); err != nil {
		fmt.Fprintf(stderr, "codeinstances shrink: %v\n", err)
		return 2
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(struct {
			*codeinstances.ShrinkResult
			File string `json:"file"`
			ID   string `json:"id"`
		}{res, *out, res.Instance.ID})
		return 0
	}
	fmt.Fprintf(stdout, "codeinstances: %d answers differ between the runs; the failure (%s) needs %d of them (a is the passing run, b the failing one):\n", res.Changed, res.Failure, len(res.Needed))
	for _, n := range res.Needed {
		fmt.Fprintf(stdout, "  - %s\n", n)
	}
	if res.FailingOnly > 0 {
		fmt.Fprintf(stdout, "codeinstances: the answers to the %d calls only the failing run made stay in as they were\n", res.FailingOnly)
	}
	if !res.Minimal {
		fmt.Fprintf(stdout, "codeinstances: stopped at the run limit (-max-runs %d); fewer answers might still do\n", *maxRuns)
	}
	if !res.Pruned {
		fmt.Fprintln(stdout, "codeinstances: the file keeps every call, since the program didn't fail again with only the calls it made")
	}
	fmt.Fprintf(stdout, "codeinstances: kept %s (%s) after %d runs\n", *out, res.Instance.Summary(), res.Runs)
	if res.Failure == "exit 0" {
		fmt.Fprintln(stdout, "codeinstances: the failure exits 0, so run it as a test with -expect pass")
	}
	return 0
}

func test(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(stderr)
	expect := fs.String("expect", "auto", "what each replay must do: match (replay exactly), pass (exit 0), or auto (match for runs recorded with exit 0, pass for the rest)")
	noStderr := fs.Bool("ignore-stderr", false, "leave stderr out when matching")
	timeout := fs.Duration("timeout", time.Minute, "kill the program after this long, in each test (0: no limit)")
	asJSON := fs.Bool("json", false, "print the results as JSON")
	verbose := fs.Bool("v", false, "list the notes of passing tests too")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: codeinstances test [flags] file-or-dir... [-- command [args...]]")
		fs.PrintDefaults()
	}
	flagArgs, command := splitCommand(args)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	switch *expect {
	case codeinstances.ExpectAuto, codeinstances.ExpectMatch, codeinstances.ExpectPass:
	default:
		fmt.Fprintf(stderr, "codeinstances test: -expect %q: want auto, match or pass\n", *expect)
		return 2
	}
	files, err := instanceFiles(fs.Args())
	if err != nil {
		fmt.Fprintf(stderr, "codeinstances test: %v\n", err)
		return 2
	}
	if len(files) == 0 {
		fmt.Fprintln(stderr, "codeinstances test: no .instance files found")
		return 2
	}
	opts := codeinstances.TestOptions{Argv: command, Expect: *expect, IgnoreStderr: *noStderr, Timeout: *timeout}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	type entry struct {
		File string `json:"file"`
		*codeinstances.TestResult
	}
	var results []entry
	failed := 0
	for _, f := range files {
		inst, err := instance.Load(f)
		if err != nil {
			fmt.Fprintf(stderr, "codeinstances test: %v\n", err)
			return 2
		}
		r, err := codeinstances.TestInstance(ctx, inst, opts)
		if err != nil {
			fmt.Fprintf(stderr, "codeinstances test: %s: %v\n", f, err)
			return 2
		}
		results = append(results, entry{f, r})
		if !r.Passed {
			failed++
		}
		if *asJSON {
			continue
		}
		what := "matched"
		if r.Expect == codeinstances.ExpectPass {
			what = fmt.Sprintf("%s (recorded %s)", r.Replayed, r.Recorded)
		}
		if r.Passed {
			fmt.Fprintf(stdout, "ok    %s  %s, %d ms\n", f, what, r.DurationMS)
		} else {
			fmt.Fprintf(stdout, "FAIL  %s  %d ms\n", f, r.DurationMS)
		}
		for _, p := range r.Problems {
			fmt.Fprintf(stdout, "        - %s\n", p)
		}
		if *verbose {
			for _, n := range r.Notes {
				fmt.Fprintf(stdout, "        note: %s\n", n)
			}
		}
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(results)
	} else {
		fmt.Fprintf(stdout, "codeinstances: %d of %d tests passed\n", len(files)-failed, len(files))
	}
	if failed > 0 {
		return 1
	}
	return 0
}

// instanceFiles lists the files given, and the .instance files under each
// directory given, in name order, each once.
func instanceFiles(paths []string) ([]string, error) {
	var files []string
	seen := map[string]bool{}
	add := func(f string) {
		if !seen[filepath.Clean(f)] {
			seen[filepath.Clean(f)] = true
			files = append(files, f)
		}
	}
	for _, p := range paths {
		fi, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if !fi.IsDir() {
			add(p)
			continue
		}
		err = filepath.WalkDir(p, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(d.Name(), ".instance") {
				add(path)
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return files, nil
}

func group(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("group", flag.ContinueOnError)
	fs.SetOutput(stderr)
	asJSON := fs.Bool("json", false, "print the groups as JSON")
	all := fs.Bool("v", false, "list every run in each group, not only the first 5")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: codeinstances group [flags] file-or-dir...")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	files, err := instanceFiles(fs.Args())
	if err != nil {
		fmt.Fprintf(stderr, "codeinstances group: %v\n", err)
		return 2
	}
	if len(files) == 0 {
		fmt.Fprintln(stderr, "codeinstances group: no .instance files found")
		return 2
	}
	var runs []codeinstances.NamedInstance
	seen := map[string]bool{}
	for _, f := range files {
		inst, err := instance.Load(f)
		if err != nil {
			fmt.Fprintf(stderr, "codeinstances group: %v\n", err)
			return 2
		}
		// The same file given twice, or two copies of it, count once.
		if seen[inst.ID] {
			continue
		}
		seen[inst.ID] = true
		runs = append(runs, codeinstances.NamedInstance{Name: f, Instance: inst})
	}
	g := codeinstances.GroupFailures(runs)
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		enc.Encode(g)
		return 0
	}
	fmt.Fprintf(stdout, "codeinstances: %s in %s (%s passed)\n", plural(g.Failing, "failing run"), plural(len(g.Groups), "group"), plural(g.Passed, "run"))
	for i, grp := range g.Groups {
		fp := grp.Fingerprint
		when := ""
		if grp.First != nil {
			when = "  " + grp.First.UTC().Format("2006-01-02 15:04 UTC")
			if last := grp.Last.UTC().Format("2006-01-02 15:04 UTC"); last != when[2:] {
				when += " to " + last
			}
		}
		fmt.Fprintf(stdout, "\n%d. %s  %s%s\n", i+1, plural(len(grp.Runs), "run"), fp.ID, when)
		fmt.Fprintf(stdout, "   ended  %s\n", fp.Ended)
		for j, l := range strings.Split(fp.Error, "\n") {
			if l == "" {
				continue
			}
			label := "error "
			if j > 0 {
				label = "      "
			}
			fmt.Fprintf(stdout, "   %s %s\n", label, l)
		}
		for _, c := range fp.Calls {
			fmt.Fprintf(stdout, "   call   %s\n", c)
		}
		for j, r := range grp.Runs {
			if j == 5 && !*all {
				fmt.Fprintf(stdout, "   ... and %d more (-v lists them)\n", len(grp.Runs)-5)
				break
			}
			fmt.Fprintf(stdout, "   - %s\n", r)
		}
	}
	return 0
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

func report(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("report", flag.ContinueOnError)
	fs.SetOutput(stderr)
	storeURL := fs.String("url", os.Getenv("CODEINSTANCES_STORE"), "the store the instance was pushed to, so the report says how to pull it (default $CODEINSTANCES_STORE)")
	lines := fs.Int("lines", 20, "how many lines of output to show")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: codeinstances report [flags] file")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fs.Usage()
		return 2
	}
	inst, err := instance.Load(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "codeinstances report: %v\n", err)
		return 2
	}
	fmt.Fprint(stdout, codeinstances.BugReport(inst, codeinstances.BugReportOptions{File: filepath.Base(fs.Arg(0)), StoreURL: *storeURL, TailLines: *lines}))
	return 0
}

func serve(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dir := fs.String("dir", "instances", "the folder to keep instances in")
	addr := fs.String("addr", "127.0.0.1:7979", "the address to listen on")
	token := fs.String("token", os.Getenv("CODEINSTANCES_TOKEN"), "the token clients must send (default $CODEINSTANCES_TOKEN; when neither is set, a random one is made and printed)")
	openRead := fs.Bool("open-read", false, "let anyone read without the token; uploads still need it")
	maxBytes := fs.Int64("max-size", store.DefaultMaxBytes, "the largest instance to take, in bytes")
	var hosts list
	fs.Var(&hosts, "host", "also answer to this host name, such as the public name of an HTTPS proxy in front of a loopback store (repeatable)")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: codeinstances serve [flags]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fs.Usage()
		return 2
	}
	host, _, err := net.SplitHostPort(*addr)
	if err != nil {
		fmt.Fprintf(stderr, "codeinstances serve: -addr: %v\n", err)
		return 2
	}
	made := false
	if *token == "" {
		b := make([]byte, 24)
		if _, err := rand.Read(b); err != nil {
			fmt.Fprintf(stderr, "codeinstances serve: %v\n", err)
			return 2
		}
		*token, made = hex.EncodeToString(b), true
	}
	st, err := store.Open(*dir)
	if err != nil {
		fmt.Fprintf(stderr, "codeinstances serve: %v\n", err)
		return 2
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		fmt.Fprintf(stderr, "codeinstances serve: %v\n", err)
		return 2
	}
	fmt.Fprintf(stderr, "codeinstances: serving %s (%d instances) at http://%s\n", *dir, len(st.List()), ln.Addr())
	if made {
		fmt.Fprintf(stderr, "codeinstances: token %s (set CODEINSTANCES_TOKEN or -token to choose one)\n", *token)
	}
	handler := &store.Server{Store: st, Token: *token, OpenRead: *openRead, MaxBytes: *maxBytes}
	if isLoopback(host) {
		// Only loopback names, so a web page can't reach the store by
		// pointing a name of its own at 127.0.0.1.
		handler.Hosts = append([]string{"127.0.0.1", "localhost", "::1", host}, hosts...)
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      2 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdown)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Fprintf(stderr, "codeinstances serve: %v\n", err)
		return 2
	}
	return 0
}

func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func storeClient(fs *flag.FlagSet, urlFlag string) (*store.Client, *string, *string) {
	u := fs.String(urlFlag, os.Getenv("CODEINSTANCES_STORE"), "the store's address, such as http://127.0.0.1:7979 (default $CODEINSTANCES_STORE)")
	t := fs.String("token", os.Getenv("CODEINSTANCES_TOKEN"), "the store's token (default $CODEINSTANCES_TOKEN)")
	return &store.Client{}, u, t
}

func push(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	fs.SetOutput(stderr)
	client, u, t := storeClient(fs, "to")
	asJSON := fs.Bool("json", false, "print the results as JSON")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: codeinstances push [flags] file...")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() == 0 || *u == "" {
		if *u == "" {
			fmt.Fprintln(stderr, "codeinstances push: give -to or set CODEINSTANCES_STORE")
		}
		fs.Usage()
		return 2
	}
	client.URL, client.Token = *u, *t
	files, err := instanceFiles(fs.Args())
	if err != nil {
		fmt.Fprintf(stderr, "codeinstances push: %v\n", err)
		return 2
	}
	type entry struct {
		File string `json:"file"`
		*store.Pushed
	}
	var results []entry
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			fmt.Fprintf(stderr, "codeinstances push: %v\n", err)
			return 2
		}
		p, err := client.Push(data)
		if err != nil {
			fmt.Fprintf(stderr, "codeinstances push: %s: %v\n", f, err)
			return 2
		}
		results = append(results, entry{f, p})
		if !*asJSON {
			state := "new"
			if !p.Created {
				state = "already there"
			}
			fmt.Fprintf(stdout, "%s  %s  %s (%s)\n", f, p.ID, codeinstances.SafeText(p.URL), state)
		}
	}
	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(results)
	}
	return 0
}

func pull(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("pull", flag.ContinueOnError)
	fs.SetOutput(stderr)
	client, u, t := storeClient(fs, "from")
	out := fs.String("o", "", "the file to write (default: the first 12 digits of the id, then .instance)")
	maxBytes := fs.Int64("max-size", store.DefaultMaxBytes, "the largest instance to take, in bytes")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: codeinstances pull [flags] id")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 || *u == "" {
		if *u == "" {
			fmt.Fprintln(stderr, "codeinstances pull: give -from or set CODEINSTANCES_STORE")
		}
		fs.Usage()
		return 2
	}
	client.URL, client.Token, client.MaxBytes = *u, *t, *maxBytes
	data, inst, err := client.Pull(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "codeinstances pull: %v\n", err)
		return 2
	}
	path := *out
	if path == "" {
		path = instance.ShortID(inst.ID) + ".instance"
	}
	if err := instance.WriteAtomic(path, data); err != nil {
		fmt.Fprintf(stderr, "codeinstances pull: %v\n", err)
		return 2
	}
	fmt.Fprintf(stdout, "codeinstances: pulled %s (%s)\n", path, codeinstances.SafeText(inst.Summary()))
	return 0
}

func fork(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fork", flag.ContinueOnError)
	fs.SetOutput(stderr)
	at := fs.Int("at", 0, "the parent's first call not to answer from the parent (required); one past the last answers them all")
	out := fs.String("o", "fork.instance", "the instance file to write")
	answerFile := fs.String("answer", "", "a JSON file with the answer to give at the step instead of sending it, such as {\"status\": 503, \"body\": \"busy\"}")
	offline := fs.Bool("offline", false, "send nothing: requests the parent has no answer for get a 502")
	fresh := fs.Bool("fresh-values", false, "hand the program new clock and random values instead of the parent's")
	var resolve, cas list
	fs.Var(&resolve, "resolve", "send host[:port] to another address, as host=addr[:port] (repeatable)")
	fs.Var(&cas, "upstream-ca", "also trust this PEM file's certificates when reaching real HTTPS servers (repeatable)")
	keep := fs.Bool("keep-secrets", false, "keep Authorization, Cookie and similar headers instead of redacting them")
	timeout := fs.Duration("timeout", 0, "kill the program after this long (0: no limit)")
	quiet := fs.Bool("q", false, "don't pass the program's output through")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "usage: codeinstances fork [flags] -at N parent.instance [-- command [args...]]")
		fs.PrintDefaults()
	}
	flagArgs, command := splitCommand(args)
	if err := fs.Parse(flagArgs); err != nil {
		return 2
	}
	if fs.NArg() != 1 || *at == 0 {
		fs.Usage()
		return 2
	}
	parent, err := instance.Load(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "codeinstances fork: %v\n", err)
		return 2
	}
	opts := codeinstances.ForkOptions{At: *at, Offline: *offline, FreshValues: *fresh, Argv: command, KeepSecrets: *keep, Timeout: *timeout, Resolve: map[string]string{}}
	if *answerFile != "" {
		a, err := codeinstances.ReadAnswer(*answerFile)
		if err != nil {
			fmt.Fprintf(stderr, "codeinstances fork: -answer: %v\n", err)
			return 2
		}
		opts.Answer = a
	}
	for _, r := range resolve {
		host, addr, ok := strings.Cut(r, "=")
		if !ok || host == "" || addr == "" {
			fmt.Fprintf(stderr, "codeinstances fork: -resolve %q: want host=addr\n", r)
			return 2
		}
		opts.Resolve[host] = addr
	}
	for _, path := range cas {
		pem, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(stderr, "codeinstances fork: %v\n", err)
			return 2
		}
		opts.UpstreamCAs = append(opts.UpstreamCAs, pem)
	}
	if !*quiet {
		opts.Stdout, opts.Stderr = stdout, stderr
	}
	ctx, stop := interrupts()
	defer stop()
	f, err := codeinstances.Fork(ctx, parent, opts)
	if err != nil {
		fmt.Fprintf(stderr, "codeinstances fork: %v\n", err)
		return 2
	}
	if err := instance.Save(*out, f); err != nil {
		fmt.Fprintf(stderr, "codeinstances fork: %v\n", err)
		return 2
	}
	sum := codeinstances.Summarize(f)
	fmt.Fprintf(stderr, "codeinstances: forked %s at call %d: %d answered from the parent, %d given, %d sent, %d not sent (offline)\n",
		instance.ShortID(parent.ID), *at, sum.FromParent, sum.Given, sum.Sent, sum.Offline)
	fmt.Fprintf(stderr, "codeinstances: kept %s (%s)\n", *out, codeinstances.SafeText(f.Summary()))
	if f.Result.ExitCode >= 0 {
		return f.Result.ExitCode
	}
	return 1
}
