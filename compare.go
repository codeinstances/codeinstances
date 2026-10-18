// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package codeinstances

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/codeinstances/codeinstances/instance"
)

// CompareOptions says what Compare leaves out.
type CompareOptions struct {
	// IgnoreStderr leaves stderr out of the comparison.
	IgnoreStderr bool
	// Values compares the clock and random values themselves. By default
	// only how many of each kind the runs asked for is compared, since two
	// separate runs read different clocks and draw different numbers.
	Values bool
}

// Comparison is what Compare found between two instances, a and b.
type Comparison struct {
	// Same is true when there are no differences. Notes and speed don't count.
	Same        bool     `json:"same"`
	Differences []string `json:"differences"`
	// Notes are differences in how the runs were made (the command, the kept
	// environment, the tool) that are shown but not counted.
	Notes []string    `json:"notes"`
	A     Side        `json:"a"`
	B     Side        `json:"b"`
	Calls CallCounts  `json:"calls"`
	Speed SpeedReport `json:"speed"`
}

// Side describes one of the two instances.
type Side struct {
	ID         string    `json:"id"`
	Created    time.Time `json:"created"`
	Tool       string    `json:"tool"`
	Argv       []string  `json:"argv"`
	Ended      string    `json:"ended"`
	DurationMS int64     `json:"duration_ms"`
	Calls      int       `json:"calls"`
	Values     int       `json:"values"`
}

// CallCounts says how the calls of a and b paired up.
type CallCounts struct {
	// Same is the number of pairs that came out identical.
	Same int `json:"same"`
	// Changed is the number of pairs that differ in their request body or
	// in what came back.
	Changed int `json:"changed"`
	OnlyA   int `json:"only_a"`
	OnlyB   int `json:"only_b"`
}

// SpeedReport compares how long the runs and their calls took.
type SpeedReport struct {
	ADurationMS int64 `json:"a_duration_ms"`
	BDurationMS int64 `json:"b_duration_ms"`
	// Calls holds the paired calls whose time is known in both runs (it is
	// kept from version 0.2.0 on), largest change first.
	Calls []CallSpeed `json:"calls"`
}

// CallSpeed is one paired call's time in a and in b.
type CallSpeed struct {
	Method string `json:"method"`
	URL    string `json:"url"`
	ASeq   int    `json:"a_seq"`
	BSeq   int    `json:"b_seq"`
	AMS    int64  `json:"a_ms"`
	BMS    int64  `json:"b_ms"`
}

// Compare finds how two instances differ: how the runs ended, what they
// printed, the calls they made and what came back, and the values they asked
// for. Calls are paired by method, URL and request body, in order; calls left
// over are then paired by method and URL alone, which shows a changed
// request body as a change rather than as one call missing and one extra.
// Headers aren't compared, since they carry dates and other noise.
func Compare(a, b *instance.Instance, opts CompareOptions) *Comparison {
	c := &Comparison{Differences: []string{}, Notes: []string{}, Speed: SpeedReport{Calls: []CallSpeed{}}}
	c.A, c.B = side(a), side(b)
	c.Speed.ADurationMS, c.Speed.BDurationMS = a.Result.DurationMS, b.Result.DurationMS

	if !equalStrings(a.Command.Argv, b.Command.Argv) {
		c.Notes = append(c.Notes, fmt.Sprintf("the commands differ: a ran %s, b ran %s", argvString(a.Command.Argv), argvString(b.Command.Argv)))
	}
	if d := envDiff(a.Command.Env, b.Command.Env); d != "" {
		c.Notes = append(c.Notes, d)
	}
	if a.Tool != b.Tool {
		c.Notes = append(c.Notes, fmt.Sprintf("recorded by different tools: a by %s, b by %s", a.Tool, b.Tool))
	}

	c.Differences = append(c.Differences, compareEndings(a.Result, b.Result, opts.IgnoreStderr)...)
	compareCalls(c, a.Calls, b.Calls)
	c.Differences = append(c.Differences, compareValueRuns(a.Values, b.Values, opts.Values)...)
	sort.SliceStable(c.Speed.Calls, func(i, j int) bool {
		return abs(c.Speed.Calls[i].BMS-c.Speed.Calls[i].AMS) > abs(c.Speed.Calls[j].BMS-c.Speed.Calls[j].AMS)
	})
	c.Same = len(c.Differences) == 0
	return c
}

func side(inst *instance.Instance) Side {
	return Side{ID: inst.ID, Created: inst.Created, Tool: inst.Tool, Argv: inst.Command.Argv, Ended: Ended(inst.Result),
		DurationMS: inst.Result.DurationMS, Calls: len(inst.Calls), Values: len(inst.Values)}
}

// Ended describes how a run ended: "exit N", "killed by SIGNAL" or
// "timed out".
func Ended(r instance.Result) string {
	switch {
	case r.TimedOut:
		return "timed out"
	case r.Signal != "":
		return "killed by " + r.Signal
	}
	return fmt.Sprintf("exit %d", r.ExitCode)
}

func compareEndings(a, b instance.Result, ignoreStderr bool) []string {
	var d []string
	if Ended(a) != Ended(b) {
		d = append(d, fmt.Sprintf("ended: a %s, b %s", Ended(a), Ended(b)))
	}
	if msg := outputDiff("stdout", a.Stdout.MustBytes(), b.Stdout.MustBytes()); msg != "" {
		d = append(d, msg)
	}
	if !ignoreStderr {
		if msg := outputDiff("stderr", a.Stderr.MustBytes(), b.Stderr.MustBytes()); msg != "" {
			d = append(d, msg)
		}
	}
	return d
}

// outputDiff names the first line where a's and b's output differ.
func outputDiff(name string, a, b []byte) string {
	if bytes.Equal(a, b) {
		return ""
	}
	if !utf8.Valid(a) || !utf8.Valid(b) {
		return name + " " + bytesDiff(a, b)
	}
	line, x, y, hx, hy := firstLineDiff(a, b)
	switch {
	case !hx && !hy:
		return name + " differs only in its last newline"
	case !hx:
		return fmt.Sprintf("%s line %d: b printed %q, which a didn't", name, line, clip(y))
	case !hy:
		return fmt.Sprintf("%s line %d: a printed %q, which b didn't", name, line, clip(x))
	}
	return fmt.Sprintf("%s line %d: a %q, b %q", name, line, clip(x), clip(y))
}

// lines splits text into lines, each ended by a newline or by the end.
func lines(text []byte) []string {
	if len(text) == 0 {
		return nil
	}
	return strings.Split(strings.TrimSuffix(string(text), "\n"), "\n")
}

// firstLineDiff returns the first line (counting from 1) where two different
// texts differ, each side's text of it, and whether each side has that line.
func firstLineDiff(a, b []byte) (int, string, string, bool, bool) {
	al, bl := lines(a), lines(b)
	for i := 0; ; i++ {
		var x, y string
		hx, hy := i < len(al), i < len(bl)
		if hx {
			x = al[i]
		}
		if hy {
			y = bl[i]
		}
		if x != y || hx != hy || (!hx && !hy) {
			return i + 1, x, y, hx, hy
		}
	}
}

func callKeyOf(c instance.Call) string {
	sum := sha256.Sum256(c.Request.Body.MustBytes())
	return c.Request.Method + " " + c.Request.URL + " " + fmt.Sprintf("%x", sum)
}

// pairCalls pairs a's calls with b's: first by method, URL and request body,
// in order, then the calls left over by method and URL alone. pair[i] is the
// index in b of a's call i, or -1; usedB marks b's paired calls.
func pairCalls(a, b []instance.Call) (pair []int, usedB []bool) {
	pair = make([]int, len(a))
	usedB = make([]bool, len(b))
	for i := range pair {
		pair[i] = -1
	}
	match := func(key func(instance.Call) string) {
		queues := map[string][]int{}
		for j, call := range b {
			if !usedB[j] {
				k := key(call)
				queues[k] = append(queues[k], j)
			}
		}
		for i, call := range a {
			if pair[i] >= 0 {
				continue
			}
			k := key(call)
			if q := queues[k]; len(q) > 0 {
				pair[i], usedB[q[0]] = q[0], true
				queues[k] = q[1:]
			}
		}
	}
	match(callKeyOf)
	match(func(call instance.Call) string { return call.Request.Method + " " + call.Request.URL })
	return pair, usedB
}

func compareCalls(c *Comparison, a, b []instance.Call) {
	pair, usedB := pairCalls(a, b)
	for i, ca := range a {
		if pair[i] < 0 {
			c.Calls.OnlyA++
			c.Differences = append(c.Differences, fmt.Sprintf("%s %s (a call %d) is only in a", ca.Request.Method, ca.Request.URL, ca.Seq))
			continue
		}
		cb := b[pair[i]]
		if ca.DurationMS != nil && cb.DurationMS != nil {
			c.Speed.Calls = append(c.Speed.Calls, CallSpeed{Method: ca.Request.Method, URL: ca.Request.URL,
				ASeq: ca.Seq, BSeq: cb.Seq, AMS: *ca.DurationMS, BMS: *cb.DurationMS})
		}
		diffs := callDiff(ca, cb)
		if len(diffs) == 0 {
			c.Calls.Same++
			continue
		}
		c.Calls.Changed++
		label := fmt.Sprintf("%s %s (a call %d, b call %d)", ca.Request.Method, ca.Request.URL, ca.Seq, cb.Seq)
		for _, d := range diffs {
			c.Differences = append(c.Differences, label+": "+d)
		}
	}
	for j, cb := range b {
		if !usedB[j] {
			c.Calls.OnlyB++
			c.Differences = append(c.Differences, fmt.Sprintf("%s %s (b call %d) is only in b", cb.Request.Method, cb.Request.URL, cb.Seq))
		}
	}
}

func callDiff(a, b instance.Call) []string {
	var d []string
	if msg := bodyDiff("request body", a.Request.Body.MustBytes(), b.Request.Body.MustBytes()); msg != "" {
		d = append(d, msg)
	}
	switch {
	case a.Error != "" && b.Error != "":
		if a.Error != b.Error {
			d = append(d, fmt.Sprintf("error: a %q, b %q", a.Error, b.Error))
		}
		return d
	case a.Error != "":
		return append(d, fmt.Sprintf("a got no answer (%s), b got status %d", SafeText(a.Error), b.Response.Status))
	case b.Error != "":
		return append(d, fmt.Sprintf("a got status %d, b got no answer (%s)", a.Response.Status, SafeText(b.Error)))
	}
	if a.Response.Status != b.Response.Status {
		d = append(d, fmt.Sprintf("status: a %d, b %d", a.Response.Status, b.Response.Status))
	}
	if msg := bodyDiff("response body", a.Response.Body.MustBytes(), b.Response.Body.MustBytes()); msg != "" {
		d = append(d, msg)
	}
	return d
}

// bodyDiff says where two bodies first differ: by JSON path when both are
// JSON, by line when both are text, by size otherwise.
func bodyDiff(name string, a, b []byte) string {
	if bytes.Equal(a, b) {
		return ""
	}
	if len(a) > 0 && len(b) > 0 && json.Valid(a) && json.Valid(b) {
		va, ea := decodeJSON(a)
		vb, eb := decodeJSON(b)
		if ea == nil && eb == nil {
			if msg := jsonDiff("$", va, vb); msg != "" {
				return name + " " + msg
			}
			// The same data written differently (spacing, member order).
			return name + " is the same JSON written differently"
		}
	}
	if utf8.Valid(a) && utf8.Valid(b) {
		line, x, y, hx, hy := firstLineDiff(a, b)
		switch {
		case !hx && !hy:
			return name + " differs only in its last newline"
		case !hx:
			return fmt.Sprintf("%s line %d is only in b: %q", name, line, clip(y))
		case !hy:
			return fmt.Sprintf("%s line %d is only in a: %q", name, line, clip(x))
		}
		return fmt.Sprintf("%s line %d: a %q, b %q", name, line, clip(x), clip(y))
	}
	return name + " " + bytesDiff(a, b)
}

// bytesDiff describes two different byte strings by size and by where they
// first differ.
func bytesDiff(a, b []byte) string {
	i := 0
	for i < len(a) && i < len(b) && a[i] == b[i] {
		i++
	}
	return fmt.Sprintf("differs from byte %d (%d bytes in a, %d in b)", i+1, len(a), len(b))
}

func decodeJSON(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	err := dec.Decode(&v)
	return v, err
}

// jsonDiff returns where two JSON values first differ, or "" if they don't.
func jsonDiff(path string, a, b any) string {
	switch x := a.(type) {
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok {
			break
		}
		keys := make([]string, 0, len(x)+len(y))
		for k := range x {
			keys = append(keys, k)
		}
		for k := range y {
			if _, ok := x[k]; !ok {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := path + memberPath(k)
			xv, inA := x[k]
			yv, inB := y[k]
			switch {
			case !inB:
				return fmt.Sprintf("at %s: only in a (%s)", p, jsonText(xv))
			case !inA:
				return fmt.Sprintf("at %s: only in b (%s)", p, jsonText(yv))
			}
			if msg := jsonDiff(p, xv, yv); msg != "" {
				return msg
			}
		}
		return ""
	case []any:
		y, ok := b.([]any)
		if !ok {
			break
		}
		for i := 0; i < len(x) && i < len(y); i++ {
			if msg := jsonDiff(fmt.Sprintf("%s[%d]", path, i), x[i], y[i]); msg != "" {
				return msg
			}
		}
		if len(x) != len(y) {
			return fmt.Sprintf("at %s: %d items in a, %d in b", path, len(x), len(y))
		}
		return ""
	case json.Number:
		if y, ok := b.(json.Number); ok && x == y {
			return ""
		}
	default:
		if fmt.Sprintf("%T %v", a, a) == fmt.Sprintf("%T %v", b, b) {
			return ""
		}
	}
	return fmt.Sprintf("at %s: a %s, b %s", path, jsonText(a), jsonText(b))
}

func memberPath(k string) string {
	simple := k != ""
	for i, r := range k {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || i > 0 && r >= '0' && r <= '9') {
			simple = false
			break
		}
	}
	if simple {
		return "." + k
	}
	q, _ := json.Marshal(k)
	return "[" + string(q) + "]"
}

func jsonText(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return clip(strings.TrimSpace(buf.String()))
}

// compareValueRuns compares the values each run asked for: by default how
// many of each kind, with all set the values themselves.
func compareValueRuns(a, b []instance.Value, all bool) []string {
	var d []string
	for _, kind := range []string{instance.ValueClock, instance.ValueRandom, instance.ValueBytes} {
		va, vb := ofKind(a, kind), ofKind(b, kind)
		if len(va) != len(vb) {
			d = append(d, fmt.Sprintf("%s values: a asked for %d, b for %d", kind, len(va), len(vb)))
			continue
		}
		if !all {
			continue
		}
		for i := range va {
			if va[i] != vb[i] {
				d = append(d, fmt.Sprintf("%s value %d: a %s, b %s", kind, i+1, clip(va[i]), clip(vb[i])))
				break
			}
		}
	}
	return d
}

func ofKind(values []instance.Value, kind string) []string {
	var out []string
	for _, v := range values {
		if v.Kind == kind {
			out = append(out, v.Value)
		}
	}
	return out
}

func envDiff(a, b map[string]string) string {
	var names []string
	for k, v := range a {
		if w, ok := b[k]; !ok || w != v {
			names = append(names, k)
		}
	}
	for k := range b {
		if _, ok := a[k]; !ok {
			names = append(names, k)
		}
	}
	if len(names) == 0 {
		return ""
	}
	sort.Strings(names)
	return "the kept environment differs in " + strings.Join(names, ", ")
}

func argvString(argv []string) string {
	q := make([]string, len(argv))
	for i, s := range argv {
		if s == "" || strings.ContainsAny(s, " \t\n\"'\\$`") {
			q[i] = fmt.Sprintf("%q", s)
		} else {
			q[i] = s
		}
	}
	return strings.Join(q, " ")
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func abs(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
