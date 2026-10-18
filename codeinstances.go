// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package codeinstances keeps runs of programs as instances and replays them.
//
// Record runs a command and keeps everything that made that run what it was:
// its arguments, what it printed, how it ended, every HTTP exchange it made
// (through a local proxy) and every clock or random value it asked for
// through the hook package. Replay runs the command again with the network
// replaced by the recording and the hooked values fed back, and reports
// whether the run came out the same.
package codeinstances

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/codeinstances/codeinstances/hook"
	"github.com/codeinstances/codeinstances/instance"
	"github.com/codeinstances/codeinstances/internal/ca"
	"github.com/codeinstances/codeinstances/internal/capture"
)

// Version is this release of CodeInstances.
const Version = "0.7.0"

// MaxOutput is how much of stdout and of stderr an instance keeps.
const MaxOutput = 16 << 20

// RecordOptions says what to run and how.
type RecordOptions struct {
	Argv []string
	// Dir is the working directory; empty means the current one.
	Dir string
	// KeepEnv names environment variables to store in the instance and set
	// again on replay.
	KeepEnv []string
	// Resolve sends host names to other addresses (see capture.RecordConfig).
	Resolve map[string]string
	// UpstreamCAs are extra PEM certificates to trust when reaching real
	// HTTPS servers, on top of the system's.
	UpstreamCAs [][]byte
	// KeepSecrets keeps Authorization, Cookie and similar headers.
	KeepSecrets bool
	// Timeout kills the program after this long; zero means no limit.
	Timeout time.Duration
	// Stdout and Stderr receive the program's output as it runs.
	Stdout, Stderr io.Writer
}

// ReplayOptions says how to replay.
type ReplayOptions struct {
	// Argv runs a different command against the recording, such as a fixed
	// build of the program. Empty means the recorded command.
	Argv []string
	Dir  string
	// Strict requires HTTP requests in exactly the recorded order.
	Strict bool
	// IgnoreStderr leaves stderr out of the comparison.
	IgnoreStderr bool
	Timeout      time.Duration
	Stdout       io.Writer
	Stderr       io.Writer
}

// Report is the outcome of a replay.
type Report struct {
	Matched     bool            `json:"matched"`
	Divergences []string        `json:"divergences"`
	Recorded    instance.Result `json:"recorded"`
	Replayed    instance.Result `json:"replayed"`
	Calls       int             `json:"calls"`
	CallsServed int             `json:"calls_served"`
	// CallsUsed holds the seq numbers of the recorded calls that were served.
	CallsUsed  []int `json:"calls_used"`
	Values     int   `json:"values"`
	ValuesUsed int   `json:"values_used"`
}

// Record runs the command and returns its instance. It returns an error only
// when the command couldn't be run at all; a program that fails, crashes or
// times out is recorded like any other.
func Record(ctx context.Context, opts RecordOptions) (*instance.Instance, error) {
	if len(opts.Argv) == 0 {
		return nil, errors.New("no command to record")
	}
	auth, err := ca.New("CodeInstances recording")
	if err != nil {
		return nil, err
	}
	upstream, err := upstreamTLS(opts.UpstreamCAs)
	if err != nil {
		return nil, err
	}
	proxy, err := capture.StartRecord(capture.RecordConfig{Authority: auth, Resolve: opts.Resolve, UpstreamTLS: upstream, KeepSecrets: opts.KeepSecrets})
	if err != nil {
		return nil, err
	}
	defer proxy.Close()

	work, err := os.MkdirTemp("", "codeinstances-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)

	kept := map[string]string{}
	for _, name := range opts.KeepEnv {
		if v, ok := os.LookupEnv(name); ok {
			kept[name] = v
		}
	}
	dir := opts.Dir
	if dir == "" {
		dir, _ = os.Getwd()
	}
	created := time.Now().UTC()
	res, err := run(ctx, runSpec{
		argv: opts.Argv, dir: opts.Dir, work: work, mode: "record", proxy: proxy.URL(), caPEM: auth.CertPEM(),
		timeout: opts.Timeout, stdout: opts.Stdout, stderr: opts.Stderr,
	})
	if err != nil {
		return nil, err
	}
	// The program has exited. Closing the proxy waits for any exchange still
	// being noted, so the list of calls below is complete.
	proxy.Close()
	values, err := readValues(filepath.Join(work, "values-out"))
	if err != nil {
		return nil, err
	}
	inst := &instance.Instance{
		Format:  instance.Format,
		Created: created,
		Tool:    "codeinstances " + Version,
		Command: instance.Command{Argv: opts.Argv, Dir: dir},
		Result:  res,
		Calls:   proxy.Calls(),
	}
	if len(kept) > 0 {
		inst.Command.Env = kept
	}
	for i, l := range values {
		inst.Values = append(inst.Values, instance.Value{Seq: i + 1, Kind: l.Kind, Value: l.Value})
	}
	return inst, nil
}

// Replay runs the instance's command again against its recording and
// compares the outcome. It returns an error only when the command couldn't be
// run at all.
func Replay(ctx context.Context, inst *instance.Instance, opts ReplayOptions) (*Report, error) {
	if err := inst.Validate(); err != nil {
		return nil, err
	}
	argv := opts.Argv
	if len(argv) == 0 {
		argv = inst.Command.Argv
	}
	auth, err := ca.New("CodeInstances replay")
	if err != nil {
		return nil, err
	}
	proxy, err := capture.StartReplay(capture.ReplayConfig{Authority: auth, Calls: inst.Calls, Strict: opts.Strict})
	if err != nil {
		return nil, err
	}
	defer proxy.Close()

	work, err := os.MkdirTemp("", "codeinstances-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	if err := writeValues(filepath.Join(work, "values-in"), inst.Values); err != nil {
		return nil, err
	}
	res, err := run(ctx, runSpec{
		argv: argv, dir: opts.Dir, work: work, mode: "replay", proxy: proxy.URL(), caPEM: auth.CertPEM(),
		env: inst.Command.Env, timeout: opts.Timeout, stdout: opts.Stdout, stderr: opts.Stderr,
	})
	if err != nil {
		return nil, err
	}
	proxy.Close()
	served, err := readValues(filepath.Join(work, "values-out"))
	if err != nil {
		return nil, err
	}

	rep := &Report{Recorded: inst.Result, Replayed: res, Calls: len(inst.Calls), CallsServed: proxy.Served(), CallsUsed: proxy.Used(), Values: len(inst.Values)}
	rep.Divergences = append(rep.Divergences, compareResults(inst.Result, res, opts.IgnoreStderr)...)
	rep.Divergences = append(rep.Divergences, proxy.Divergences()...)
	rep.ValuesUsed, rep.Divergences = compareValues(inst.Values, served, rep.Divergences)
	rep.Matched = len(rep.Divergences) == 0
	if rep.Divergences == nil {
		rep.Divergences = []string{}
	}
	return rep, nil
}

type runSpec struct {
	argv           []string
	dir, work      string
	mode, proxy    string
	caPEM          []byte
	env            map[string]string
	timeout        time.Duration
	stdout, stderr io.Writer
}

// proxyVars are cleared from the program's environment and set again to the
// run's proxy.
var proxyVars = []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy"}

func run(ctx context.Context, s runSpec) (instance.Result, error) {
	caFile := filepath.Join(s.work, "ca.pem")
	if err := os.WriteFile(caFile, s.caPEM, 0o600); err != nil {
		return instance.Result{}, err
	}
	if s.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, s.argv[0], s.argv[1:]...)
	cmd.Dir = s.dir
	cmd.WaitDelay = 2 * time.Second
	cmd.Env = environment(s, caFile)
	var stdout, stderr capped
	cmd.Stdout = tee(&stdout, s.stdout)
	cmd.Stderr = tee(&stderr, s.stderr)

	start := time.Now()
	if err := cmd.Start(); err != nil {
		return instance.Result{}, err
	}
	waitErr := cmd.Wait()
	res := instance.Result{
		DurationMS:      time.Since(start).Milliseconds(),
		Stdout:          instance.NewBody(stdout.buf.Bytes()),
		Stderr:          instance.NewBody(stderr.buf.Bytes()),
		StdoutTruncated: stdout.cut,
		StderrTruncated: stderr.cut,
	}
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
	}
	if ps := cmd.ProcessState; ps != nil {
		res.ExitCode = ps.ExitCode()
		if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			res.Signal = ws.Signal().String()
		}
	} else if waitErr != nil {
		return instance.Result{}, waitErr
	}
	return res, nil
}

// envName is how the system compares variable names: Windows ignores case.
func envName(k string) string {
	if runtime.GOOS == "windows" {
		return strings.ToUpper(k)
	}
	return k
}

func environment(s runSpec, caFile string) []string {
	skip := map[string]bool{}
	for _, k := range proxyVars {
		// Proxy variables are cleared in either case on every system.
		skip[strings.ToUpper(k)] = true
	}
	for _, k := range []string{hook.EnvMode, hook.EnvValuesIn, hook.EnvValuesOut, hook.EnvCA} {
		skip[envName(k)] = true
	}
	for k := range s.env {
		skip[envName(k)] = true
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if isProxyVar(k) || skip[envName(k)] {
			continue
		}
		env = append(env, kv)
	}
	for k, v := range s.env {
		// A kept variable can't steer the program around the proxy or the
		// hook: NO_PROXY=* would let an offline fork or a replay send
		// requests.
		if isProxyVar(k) || isHookVar(k) {
			continue
		}
		env = append(env, k+"="+v)
	}
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "http_proxy", "https_proxy"} {
		env = append(env, k+"="+s.proxy)
	}
	env = append(env, "NO_PROXY=", "no_proxy=", "ALL_PROXY=", "all_proxy=")
	env = append(env,
		hook.EnvMode+"="+s.mode,
		hook.EnvValuesOut+"="+filepath.Join(s.work, "values-out"),
		hook.EnvCA+"="+caFile,
		// Common ways to tell a program which certificate authority to trust.
		"SSL_CERT_FILE="+caFile,
		"REQUESTS_CA_BUNDLE="+caFile,
		"CURL_CA_BUNDLE="+caFile,
		"NODE_EXTRA_CA_CERTS="+caFile,
	)
	if s.mode == "replay" {
		env = append(env, hook.EnvValuesIn+"="+filepath.Join(s.work, "values-in"))
	}
	return dedupe(env)
}

func isHookVar(k string) bool {
	return strings.HasPrefix(strings.ToUpper(k), "CODEINSTANCES_")
}

func isProxyVar(k string) bool {
	for _, p := range proxyVars {
		if strings.EqualFold(k, p) {
			return true
		}
	}
	return false
}

// dedupe keeps the last setting of each variable. Windows' per-drive
// entries ("=C:=C:\\dir") start with "=" and are kept as they are.
func dedupe(env []string) []string {
	seen := map[string]int{}
	var out []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "=") {
			out = append(out, kv)
			continue
		}
		k, _, _ := strings.Cut(kv, "=")
		k = envName(k)
		if i, ok := seen[k]; ok {
			out[i] = kv
			continue
		}
		seen[k] = len(out)
		out = append(out, kv)
	}
	return out
}

func upstreamTLS(pems [][]byte) (*tls.Config, error) {
	if len(pems) == 0 {
		return nil, nil
	}
	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	for _, p := range pems {
		if !pool.AppendCertsFromPEM(p) {
			return nil, errors.New("no certificate found in an upstream CA file")
		}
	}
	return &tls.Config{RootCAs: pool}, nil
}

func readValues(path string) ([]hook.Line, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var out []hook.Line
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	for sc.Scan() {
		var l hook.Line
		if err := json.Unmarshal(sc.Bytes(), &l); err != nil {
			// A program killed mid-write can leave half a line at the end.
			continue
		}
		switch l.Kind {
		case instance.ValueClock, instance.ValueRandom, instance.ValueBytes:
			out = append(out, l)
		}
	}
	return out, sc.Err()
}

func writeValues(path string, values []instance.Value) error {
	var b bytes.Buffer
	for _, v := range values {
		line, _ := json.Marshal(hook.Line{Kind: v.Kind, Value: v.Value})
		b.Write(line)
		b.WriteByte('\n')
	}
	return os.WriteFile(path, b.Bytes(), 0o600)
}

func compareResults(rec, rep instance.Result, ignoreStderr bool) []string {
	var d []string
	if rec.TimedOut != rep.TimedOut {
		d = append(d, fmt.Sprintf("timed out: recorded %v, replayed %v", rec.TimedOut, rep.TimedOut))
	}
	if rec.Signal != rep.Signal {
		d = append(d, fmt.Sprintf("signal: recorded %q, replayed %q", rec.Signal, rep.Signal))
	} else if rec.ExitCode != rep.ExitCode {
		d = append(d, fmt.Sprintf("exit code: recorded %d, replayed %d", rec.ExitCode, rep.ExitCode))
	}
	if msg := compareOutput("stdout", rec.Stdout.MustBytes(), rep.Stdout.MustBytes()); msg != "" {
		d = append(d, msg)
	}
	if !ignoreStderr {
		if msg := compareOutput("stderr", rec.Stderr.MustBytes(), rep.Stderr.MustBytes()); msg != "" {
			d = append(d, msg)
		}
	}
	return d
}

// compareOutput names the first line where two outputs differ.
func compareOutput(name string, a, b []byte) string {
	if bytes.Equal(a, b) {
		return ""
	}
	al := strings.Split(string(a), "\n")
	bl := strings.Split(string(b), "\n")
	for i := 0; ; i++ {
		var x, y string
		var hx, hy bool
		if i < len(al) {
			x, hx = al[i], true
		}
		if i < len(bl) {
			y, hy = bl[i], true
		}
		if !hx && !hy {
			return fmt.Sprintf("%s differs", name)
		}
		if x != y || hx != hy {
			switch {
			case !hx:
				return fmt.Sprintf("%s line %d: the replay printed %q, which the recording didn't", name, i+1, clip(y))
			case !hy:
				return fmt.Sprintf("%s line %d: the recording printed %q, which the replay didn't", name, i+1, clip(x))
			}
			return fmt.Sprintf("%s line %d: recorded %q, replayed %q", name, i+1, clip(x), clip(y))
		}
	}
}

func clip(s string) string {
	if len(s) > 120 {
		n := 117
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		return s[:n] + "..."
	}
	return s
}

// compareValues checks the values replay handed out against the recording:
// made-up ones mean the program asked for more than was recorded; unused
// ones mean it asked for fewer.
func compareValues(recorded []instance.Value, served []hook.Line, d []string) (int, []string) {
	want := map[string]int{}
	for _, v := range recorded {
		want[v.Kind]++
	}
	used := map[string]int{}
	live := map[string]int{}
	total := 0
	for _, l := range served {
		if l.Live {
			live[l.Kind]++
		} else {
			used[l.Kind]++
			total++
		}
	}
	for _, kind := range []string{instance.ValueClock, instance.ValueRandom, instance.ValueBytes} {
		if live[kind] > 0 {
			d = append(d, fmt.Sprintf("the program asked for %d more %s values than the %d recorded", live[kind], kind, want[kind]))
		}
		if used[kind] < want[kind] {
			d = append(d, fmt.Sprintf("the program used %d of %d recorded %s values", used[kind], want[kind], kind))
		}
	}
	return total, d
}

// capped keeps the first MaxOutput bytes written to it.
type capped struct {
	buf bytes.Buffer
	cut bool
}

func (c *capped) Write(p []byte) (int, error) {
	room := MaxOutput - c.buf.Len()
	if room < len(p) {
		c.cut = true
		if room > 0 {
			c.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return c.buf.Write(p)
}

func tee(c *capped, w io.Writer) io.Writer {
	if w == nil {
		return c
	}
	return io.MultiWriter(c, quiet{w})
}

// quiet passes output on and ignores errors, so a closed terminal doesn't
// change what the program sees.
type quiet struct{ w io.Writer }

func (q quiet) Write(p []byte) (int, error) {
	q.w.Write(p)
	return len(p), nil
}
