// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

// Command codeinstances records runs of programs as instances and replays
// them.
//
//	codeinstances record [flags] -- command [args...]
//	codeinstances replay [flags] file [-- command [args...]]
//	codeinstances show [-json] file
//	codeinstances verify file
//	codeinstances version [-json]
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/codeinstances/codeinstances"
	"github.com/codeinstances/codeinstances/instance"
	"github.com/codeinstances/codeinstances/internal/buildinfo"
)

const usage = `CodeInstances keeps runs of programs as instances and replays them.

Usage:
  codeinstances record [flags] -- command [args...]
      Run the command and keep the run as an instance file: its output, how
      it ended, every HTTP(S) exchange and every hooked clock or random value.
  codeinstances replay [flags] file [-- command [args...]]
      Run the recorded command again (or the one given) with the network
      replaced by the recording, and report whether it came out the same.
  codeinstances show [-json] file
      Describe an instance.
  codeinstances verify file
      Check that an instance file hasn't been changed since it was recorded.
  codeinstances version [-json]

Run "codeinstances record -h" or "codeinstances replay -h" for their flags.
Exit status: record exits as the program did; replay exits 0 when the run
matched, 1 when it diverged; anything else is 2 on a usage or file error.
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
	inst, err := instance.Load(fs.Arg(0))
	if err != nil {
		fmt.Fprintf(stderr, "codeinstances show: %v\n", err)
		return 2
	}
	if *asJSON {
		data, _ := instance.Marshal(inst)
		stdout.Write(data)
		return 0
	}
	r := inst.Result
	fmt.Fprintf(stdout, "instance  %s\n", inst.ID)
	fmt.Fprintf(stdout, "recorded  %s by %s\n", inst.Created.Format(time.RFC3339), inst.Tool)
	fmt.Fprintf(stdout, "command   %s\n", strings.Join(quoteArgs(inst.Command.Argv), " "))
	switch {
	case r.TimedOut:
		fmt.Fprintf(stdout, "ended     timed out after %d ms\n", r.DurationMS)
	case r.Signal != "":
		fmt.Fprintf(stdout, "ended     killed by %s after %d ms\n", r.Signal, r.DurationMS)
	default:
		fmt.Fprintf(stdout, "ended     exit %d after %d ms\n", r.ExitCode, r.DurationMS)
	}
	fmt.Fprintf(stdout, "output    %d bytes on stdout, %d on stderr\n", len(r.Stdout.MustBytes()), len(r.Stderr.MustBytes()))
	fmt.Fprintf(stdout, "calls     %d\n", len(inst.Calls))
	for _, c := range inst.Calls {
		if c.Error != "" {
			fmt.Fprintf(stdout, "  %3d  %6d ms  %s %s -> error: %s\n", c.Seq, c.AtMS, c.Request.Method, c.Request.URL, c.Error)
		} else {
			fmt.Fprintf(stdout, "  %3d  %6d ms  %s %s -> %d (%d bytes)\n", c.Seq, c.AtMS, c.Request.Method, c.Request.URL, c.Response.Status, len(c.Response.Body.MustBytes()))
		}
	}
	counts := map[string]int{}
	for _, v := range inst.Values {
		counts[v.Kind]++
	}
	fmt.Fprintf(stdout, "values    %d (clock %d, random %d, bytes %d)\n", len(inst.Values), counts[instance.ValueClock], counts[instance.ValueRandom], counts[instance.ValueBytes])
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

func quoteArgs(argv []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if a == "" || strings.ContainsAny(a, " \t\n\"'\\$`") {
			out[i] = fmt.Sprintf("%q", a)
		} else {
			out[i] = a
		}
	}
	return out
}
