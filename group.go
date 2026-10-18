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

// FingerprintVersion is the prefix of fingerprint IDs. It changes whenever
// the way fingerprints are made changes, so IDs from different versions are
// never mistaken for each other.
const FingerprintVersion = "fp1"

// Fingerprint is what a failing run's failure looks like with the details
// that change from one repeat to the next (numbers, ids, times) blanked out.
// Two runs that fail the same way get the same ID.
type Fingerprint struct {
	// ID is FingerprintVersion, a colon and the first 16 hex digits of the
	// SHA-256 of the parts.
	ID string `json:"id"`
	// Ended is how the run ended, such as "exit 1" or "timed out".
	Ended string `json:"ended"`
	// Error is the run's error, normalised. See errorSignature.
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
	fp.ID = FingerprintVersion + ":" + hex.EncodeToString(h.Sum(nil))[:16]
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
	// Runs are in the order they were recorded.
	Runs []string `json:"runs"`
	// First and Last are when the earliest and latest runs were recorded;
	// absent when no run has a time.
	First *time.Time `json:"first,omitempty"`
	Last  *time.Time `json:"last,omitempty"`
}

// Grouping is what GroupFailures found.
type Grouping struct {
	Groups []Group `json:"groups"`
	// Failing and Passed count the runs.
	Failing int `json:"failing"`
	Passed  int `json:"passed"`
}

// GroupFailures puts failing runs with the same fingerprint together: the
// biggest group first, then the one seen last. Runs that passed are counted
// and left out.
func GroupFailures(runs []NamedInstance) *Grouping {
	fr := make([]FingerprintedRun, len(runs))
	for i, r := range runs {
		fr[i] = FingerprintedRun{Name: r.Name, Created: r.Instance.Created, Failed: Failed(r.Instance)}
		if fr[i].Failed {
			fr[i].Fingerprint = FingerprintOf(r.Instance)
		}
	}
	return GroupFingerprints(fr)
}

// FingerprintedRun is a run whose fingerprint is already known, for
// grouping without reading the instance again.
type FingerprintedRun struct {
	Name        string
	Created     time.Time
	Failed      bool
	Fingerprint Fingerprint
}

// GroupFingerprints is GroupFailures for runs fingerprinted already.
func GroupFingerprints(runs []FingerprintedRun) *Grouping {
	g := &Grouping{Groups: []Group{}}
	index := map[string]int{}
	var members [][]FingerprintedRun
	for _, r := range runs {
		if !r.Failed {
			g.Passed++
			continue
		}
		g.Failing++
		i, ok := index[r.Fingerprint.ID]
		if !ok {
			i = len(g.Groups)
			index[r.Fingerprint.ID] = i
			g.Groups = append(g.Groups, Group{Fingerprint: r.Fingerprint})
			members = append(members, nil)
		}
		members[i] = append(members[i], r)
	}
	for i := range g.Groups {
		m := members[i]
		sort.SliceStable(m, func(a, b int) bool { return m[a].Created.Before(m[b].Created) })
		grp := &g.Groups[i]
		for _, r := range m {
			grp.Runs = append(grp.Runs, r.Name)
			t := r.Created
			if t.IsZero() {
				continue
			}
			if grp.First == nil || t.Before(*grp.First) {
				grp.First = &t
			}
			if grp.Last == nil || t.After(*grp.Last) {
				grp.Last = &t
			}
		}
	}
	last := func(gr Group) time.Time {
		if gr.Last == nil {
			return time.Time{}
		}
		return *gr.Last
	}
	sort.SliceStable(g.Groups, func(i, j int) bool {
		a, b := g.Groups[i], g.Groups[j]
		if len(a.Runs) != len(b.Runs) {
			return len(a.Runs) > len(b.Runs)
		}
		if !last(a).Equal(last(b)) {
			return last(a).After(last(b))
		}
		return a.Fingerprint.ID < b.Fingerprint.ID
	})
	return g
}

var (
	ansiCSI     = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	ansiOSC     = regexp.MustCompile(`\x1b\][^\x07\x1b]*(\x07|\x1b\\)?`)
	goFrame     = regexp.MustCompile(`^([\w./*()\-\[\]{}]+)\(.*\)$`)
	goTestFail  = regexp.MustCompile(`^\s*--- FAIL: (\S+)`)
	pyFrame     = regexp.MustCompile(`^\s*File "([^"]*)", line \d+, in (\S+)`)
	atFrame     = regexp.MustCompile(`^\s+at \S`)
	frameLineNo = regexp.MustCompile(`:\d+(:\d+)?\)?$`)
	errorish    = regexp.MustCompile(`(?i)error|fail|fatal|exception|panic|refused|denied|timed? ?out|cannot|can't|unable|invalid|not found`)
	trailer     = regexp.MustCompile(`^(FAIL(\s|$)|ok\s|exit status \d+$|make(\[\d+\])?: \*\*\*|Node\.js v\d|\.\.\. \d+ more$|[-+=|*~_ ]+$)`)
)

// cleanOutput turns a program's output into plain lines: escape sequences
// (colours, window titles) and other control characters removed, and a
// carriage return taken as a line break.
func cleanOutput(out []byte) []string {
	text := ansiOSC.ReplaceAllString(ansiCSI.ReplaceAllString(string(out), ""), "")
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	text = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 && r != '\n' && r != '\t', r >= 0x7f && r <= 0x9f:
			return -1 // C0 and C1 controls, DEL
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069:
			return -1 // bidi overrides, which can reorder what a reader sees
		}
		return r
	}, text)
	return strings.Split(strings.TrimRight(text, "\n"), "\n")
}

// errorSignature picks the error out of a program's output, in this order:
//   - a Go panic or fatal error: its message and the first three functions
//     of the failing goroutine past the panic, outside the runtime and the
//     testing package;
//   - a Python traceback: the exception and its last three frames;
//   - an error followed by "at" frames (Node.js, Java, C#): the error and
//     its first three frames, without line numbers;
//   - failed Go tests: the "--- FAIL:" test names;
//   - else the last line that looks like an error, or the last line, skipping
//     trailers such as "exit status 1" or a bare "FAIL".
//
// Messages are normalised; frame and test names are kept as they are.
func errorSignature(out []byte) string {
	lines := cleanOutput(out)
	if sig := goPanicSignature(lines); sig != "" {
		return sig
	}
	if sig := pythonSignature(lines); sig != "" {
		return sig
	}
	if sig := atFramesSignature(lines); sig != "" {
		return sig
	}
	var failed []string
	for _, l := range lines {
		if m := goTestFail.FindStringSubmatch(l); m != nil {
			failed = append(failed, "--- FAIL: "+m[1])
		}
	}
	if len(failed) > 0 {
		if len(failed) > 5 {
			failed = append(failed[:5], "...")
		}
		return strings.Join(failed, "\n")
	}
	lastLine := ""
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if l == "" || trailer.MatchString(l) {
			continue
		}
		if errorish.MatchString(l) {
			return normalize(l)
		}
		if lastLine == "" {
			lastLine = l
		}
	}
	return normalize(lastLine)
}

func goPanicSignature(lines []string) string {
	for i, l := range lines {
		if !strings.HasPrefix(l, "panic: ") && !strings.HasPrefix(l, "fatal error: ") {
			continue
		}
		msg := normalize(strings.TrimSuffix(strings.TrimSpace(l), " [recovered]"))
		// The failing goroutine's stack: from its "goroutine N [...]:" line to
		// the next blank line.
		var frames []string
		inStack := false
		for _, f := range lines[i+1:] {
			if !inStack {
				inStack = strings.HasPrefix(f, "goroutine ")
				continue
			}
			if strings.TrimSpace(f) == "" {
				break
			}
			if m := goFrame.FindStringSubmatch(f); m != nil {
				frames = append(frames, m[1])
			}
		}
		// Frames after the last panic call are where it happened.
		for k := len(frames) - 1; k >= 0; k-- {
			if frames[k] == "panic" {
				frames = frames[k+1:]
				break
			}
		}
		sig := []string{msg}
		for _, f := range frames {
			if len(sig) == 4 {
				break
			}
			if !strings.HasPrefix(f, "runtime.") && !strings.HasPrefix(f, "testing.") {
				sig = append(sig, "at "+f)
			}
		}
		return strings.Join(sig, "\n")
	}
	return ""
}

func pythonSignature(lines []string) string {
	for i := len(lines) - 1; i >= 0; i-- {
		if !strings.HasPrefix(lines[i], "Traceback (most recent call last)") {
			continue
		}
		var frames []string
		exc := ""
		for _, l := range lines[i+1:] {
			if m := pyFrame.FindStringSubmatch(l); m != nil {
				frames = append(frames, "at "+m[2]+" ("+baseName(m[1])+")")
			} else if exc == "" && len(frames) > 0 && l != "" && l[0] != ' ' && l[0] != '\t' {
				// The first unindented line after the frames is the exception.
				exc = normalize(l)
			}
		}
		if exc == "" {
			return ""
		}
		if len(frames) > 3 {
			frames = frames[len(frames)-3:]
		}
		// Innermost frame first, as for Go.
		for a, b := 0, len(frames)-1; a < b; a, b = a+1, b-1 {
			frames[a], frames[b] = frames[b], frames[a]
		}
		return strings.Join(append([]string{exc}, frames...), "\n")
	}
	return ""
}

func atFramesSignature(lines []string) string {
	for i := 0; i+1 < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "" || atFrame.MatchString(lines[i]) || !atFrame.MatchString(lines[i+1]) {
			continue
		}
		sig := []string{normalize(lines[i])}
		for _, f := range lines[i+1:] {
			if len(sig) == 4 || !atFrame.MatchString(f) {
				break
			}
			f = frameLineNo.ReplaceAllString(strings.TrimSpace(f), "")
			// Keep the file name of a path: "(/srv/app/x.js" becomes "(x.js".
			if open := strings.LastIndex(f, "("); open >= 0 {
				f = f[:open+1] + baseName(f[open+1:])
				if !strings.HasSuffix(f, ")") {
					f += ")"
				}
			}
			sig = append(sig, f)
		}
		return strings.Join(sig, "\n")
	}
	return ""
}

var (
	months    = `(?:Jan|Feb|Mar|Apr|May|Jun|Jul|Aug|Sep|Oct|Nov|Dec)`
	timeRules = []*regexp.Regexp{
		regexp.MustCompile(`\d{4}-\d\d-\d\d[T ]\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:?\d\d)?`),
		regexp.MustCompile(`\b(?:Mon|Tue|Wed|Thu|Fri|Sat|Sun),? +` + months + ` +\d+ +\d\d:\d\d:\d\d( +\d{4})?`),
		regexp.MustCompile(`\b` + months + ` +\d+ +\d\d:\d\d:\d\d`),
		regexp.MustCompile(`\b\d\d:\d\d:\d\d(\.\d+)?\b`),
	}
	uuidRule     = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	hex0xRule    = regexp.MustCompile(`\b0x[0-9a-fA-F]+\b`)
	durationRule = regexp.MustCompile(`(^|[^A-Za-z_\d.])\d+(\.\d+)?(ns|µs|us|ms|s|m|h)\b`)
	keptCode     = regexp.MustCompile(`(?i)\b(status|code|errno|error:|returned error:?|HTTP/\d(?:\.\d)?) ?[:=]? ?(\d{3})\b`)
	alnumToken   = regexp.MustCompile(`[0-9A-Za-z]+`)
	numberRule   = regexp.MustCompile(`(^|[^A-Za-z_\d.\x01])\d+(\.\d+)?`)
)

// normalize blanks out the parts of a line that change between repeats of
// the same failure: times, UUIDs, durations, hex ids (at least 7 hex
// characters with a digit and a letter), other long ids mixing letters and
// digits, and numbers that don't follow a letter. Status codes after
// "status", "code", "errno" and "error:" are kept, since they are the
// failure.
func normalize(s string) string {
	s = strings.TrimSpace(s)
	for _, r := range timeRules {
		s = r.ReplaceAllString(s, "<time>")
	}
	s = uuidRule.ReplaceAllString(s, "<uuid>")
	s = hex0xRule.ReplaceAllString(s, "<hex>")
	s = alnumToken.ReplaceAllStringFunc(s, func(t string) string {
		digits, letters, hexOnly := 0, 0, true
		for _, c := range t {
			switch {
			case c >= '0' && c <= '9':
				digits++
			default:
				letters++
				if !(c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
					hexOnly = false
				}
			}
		}
		switch {
		case len(t) >= 7 && hexOnly && digits > 0 && letters > 0:
			return "<hex>"
		case len(t) >= 10 && digits >= 2 && letters >= 2:
			return "<id>"
		}
		return t
	})
	s = durationRule.ReplaceAllString(s, "${1}<duration>")
	// Mark kept codes so the number rule passes them by.
	s = keptCode.ReplaceAllString(s, "$1 \x01$2")
	s = numberRule.ReplaceAllString(s, "${1}<n>")
	return strings.ReplaceAll(s, "\x01", "")
}

// normalizeURL normalises the path, replaces the port (test servers pick a
// new one every run) and keeps only the names of query parameters, since
// their values are what change.
func normalizeURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return normalize(raw)
	}
	host := u.Hostname()
	if u.Port() != "" {
		host += ":<port>"
	}
	out := u.Scheme + "://" + host + normalize(u.Path)
	if u.RawQuery != "" {
		var names []string
		for k := range u.Query() {
			names = append(names, normalize(k)+"=*")
		}
		sort.Strings(names)
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
