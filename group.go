// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package codeinstances

import (
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codeinstances/codeinstances/instance"
)

// Fingerprint is what a failing run's failure looks like with the details
// that change from one repeat to the next (numbers, ids, times) blanked out.
// Two runs that fail the same way get the same ID.
type Fingerprint struct {
	// ID is "fp:" and the first 16 hex digits of the SHA-256 of the parts.
	ID string `json:"id"`
	// Ended is how the run ended, such as "exit 1" or "killed by killed".
	Ended string `json:"ended"`
	// Error is the run's error, normalised: a Go panic or Python exception
	// with the top of its stack, or else the last line on stderr (or on
	// stdout when stderr is empty).
	Error string `json:"error"`
	// Calls lists the calls that failed (no answer, or a 5xx status),
	// normalised, each once, sorted.
	Calls []string `json:"calls"`
}

// Failed says whether a run failed: it didn't exit 0.
func Failed(inst *instance.Instance) bool {
	return Ended(inst.Result) != "exit 0"
}

// FingerprintOf works out a run's fingerprint.
func FingerprintOf(inst *instance.Instance) Fingerprint {
	fp := Fingerprint{Ended: Ended(inst.Result), Calls: []string{}}
	fp.Error = errorSignature(inst.Result.Stderr.MustBytes())
	if fp.Error == "" {
		fp.Error = errorSignature(inst.Result.Stdout.MustBytes())
	}
	seen := map[string]bool{}
	for _, c := range inst.Calls {
		var what string
		switch {
		case c.Error != "":
			what = "no answer"
		case c.Response.Status >= 500:
			what = strconv.Itoa(c.Response.Status)
		default:
			continue
		}
		line := c.Request.Method + " " + normalizeURL(c.Request.URL) + " -> " + what
		if !seen[line] {
			seen[line] = true
			fp.Calls = append(fp.Calls, line)
		}
	}
	sort.Strings(fp.Calls)
	h := sha256.New()
	h.Write([]byte(fp.Ended + "\n" + fp.Error + "\n" + strings.Join(fp.Calls, "\n")))
	fp.ID = "fp:" + hex.EncodeToString(h.Sum(nil))[:16]
	return fp
}

// Summary is the fingerprint on one line.
func (fp Fingerprint) Summary() string {
	parts := []string{fp.Ended}
	if fp.Error != "" {
		parts = append(parts, clip(strings.ReplaceAll(fp.Error, "\n", " / ")))
	}
	parts = append(parts, fp.Calls...)
	return strings.Join(parts, " | ")
}

// NamedInstance is an instance and the name to report it by, such as its
// file name.
type NamedInstance struct {
	Name     string
	Instance *instance.Instance
}

// Group is the failing runs that share a fingerprint.
type Group struct {
	Fingerprint Fingerprint `json:"fingerprint"`
	Runs        []string    `json:"runs"`
	First       time.Time   `json:"first"`
	Last        time.Time   `json:"last"`
}

// Grouping is what GroupFailures found.
type Grouping struct {
	Groups []Group `json:"groups"`
	// Failing and Passed count the runs.
	Failing int `json:"failing"`
	Passed  int `json:"passed"`
}

// GroupFailures puts failing runs with the same fingerprint together, the
// biggest group first. Runs that passed are counted and left out.
func GroupFailures(runs []NamedInstance) *Grouping {
	g := &Grouping{Groups: []Group{}}
	index := map[string]int{}
	for _, r := range runs {
		if !Failed(r.Instance) {
			g.Passed++
			continue
		}
		g.Failing++
		fp := FingerprintOf(r.Instance)
		i, ok := index[fp.ID]
		if !ok {
			i = len(g.Groups)
			index[fp.ID] = i
			g.Groups = append(g.Groups, Group{Fingerprint: fp, First: r.Instance.Created, Last: r.Instance.Created})
		}
		grp := &g.Groups[i]
		grp.Runs = append(grp.Runs, r.Name)
		if r.Instance.Created.Before(grp.First) {
			grp.First = r.Instance.Created
		}
		if r.Instance.Created.After(grp.Last) {
			grp.Last = r.Instance.Created
		}
	}
	sort.SliceStable(g.Groups, func(i, j int) bool { return len(g.Groups[i].Runs) > len(g.Groups[j].Runs) })
	return g
}

var (
	goFrame     = regexp.MustCompile(`^([\w./*()\-\[\]]+)\(.*\)$`)
	pyFrame     = regexp.MustCompile(`^\s*File "([^"]*)", line \d+, in (\S+)`)
	pyException = regexp.MustCompile(`^[A-Za-z_][\w.]*(Error|Exception|Exit|Interrupt|Warning)\b`)
)

// errorSignature picks the error out of a program's output: a Go panic with
// the first three functions on its stack, a Python exception with the last
// three frames, or else the last non-empty line. The result is normalised.
func errorSignature(out []byte) string {
	text := strings.ReplaceAll(string(out), "\r\n", "\n")
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")

	// A Go panic: "panic: message", then goroutine stacks of function lines
	// each followed by a tab-indented file line.
	for i, l := range lines {
		if !strings.HasPrefix(l, "panic: ") {
			continue
		}
		sig := []string{normalize(l)}
		for _, f := range lines[i+1:] {
			if len(sig) == 4 {
				break
			}
			if m := goFrame.FindStringSubmatch(f); m != nil && !strings.HasPrefix(m[1], "panic(") && !strings.HasPrefix(m[1], "runtime.") {
				sig = append(sig, "at "+normalize(m[1]))
			}
		}
		return strings.Join(sig, "\n")
	}

	// A Python traceback: frames, then the exception on the last line.
	for i := len(lines) - 1; i >= 0; i-- {
		if !strings.HasPrefix(lines[i], "Traceback (most recent call last)") {
			continue
		}
		var frames []string
		exc := ""
		for _, l := range lines[i+1:] {
			if m := pyFrame.FindStringSubmatch(l); m != nil {
				frames = append(frames, "at "+m[2]+" ("+baseName(m[1])+")")
			} else if pyException.MatchString(l) {
				exc = normalize(l)
			}
		}
		if exc == "" {
			break
		}
		if len(frames) > 3 {
			frames = frames[len(frames)-3:]
		}
		// Innermost frame first, as in the Go case.
		for a, b := 0, len(frames)-1; a < b; a, b = a+1, b-1 {
			frames[a], frames[b] = frames[b], frames[a]
		}
		return strings.Join(append([]string{exc}, frames...), "\n")
	}

	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return normalize(l)
		}
	}
	return ""
}

var normalizers = []struct {
	re     *regexp.Regexp
	with   string
	minLen int
}{
	{regexp.MustCompile(`\d{4}-\d\d-\d\d[T ]\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:?\d\d)?`), "<time>", 0},
	{regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`), "<uuid>", 0},
	{regexp.MustCompile(`\b0x[0-9a-fA-F]+\b`), "<hex>", 0},
	// Hex words: at least 8 characters with a digit and a letter, such as ids
	// and hashes. Plain words and plain numbers aren't matched here.
	{regexp.MustCompile(`(?i)\b[0-9a-f]*(?:[0-9][0-9a-f]*[a-f]|[a-f][0-9a-f]*[0-9])[0-9a-f]*\b`), "<hex>", 8},
	{regexp.MustCompile(`\d+(\.\d+)?`), "<n>", 0},
}

// normalize blanks out the parts of a line that change between repeats of
// the same failure: times, UUIDs, hex strings and numbers.
func normalize(s string) string {
	s = strings.TrimSpace(s)
	for _, n := range normalizers {
		if n.minLen == 0 {
			s = n.re.ReplaceAllString(s, n.with)
			continue
		}
		s = n.re.ReplaceAllStringFunc(s, func(m string) string {
			if len(m) >= n.minLen {
				return n.with
			}
			return m
		})
	}
	return s
}

// normalizeURL normalises the path and keeps only the names of query
// parameters, since their values are what change.
func normalizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return normalize(raw)
	}
	out := u.Scheme + "://" + u.Host + normalize(u.Path)
	if u.RawQuery != "" {
		var names []string
		for k := range u.Query() {
			names = append(names, k)
		}
		sort.Strings(names)
		for i := range names {
			names[i] += "=*"
		}
		out += "?" + strings.Join(names, "&")
	}
	return out
}

func baseName(p string) string {
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		return p[i+1:]
	}
	return p
}
