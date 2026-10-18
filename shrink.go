// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package codeinstances

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codeinstances/codeinstances/instance"
)

// ShrinkOptions says how to shrink.
type ShrinkOptions struct {
	// Argv runs a different command against the answers, such as the build
	// that has the bug. Empty means the failing run's recorded command.
	Argv []string
	Dir  string
	// Match, when set, must also be found in a run's stdout or stderr for
	// the run to count as the same failure.
	Match *regexp.Regexp
	// Timeout limits each run of the program; zero means no limit.
	Timeout time.Duration
	// MaxRuns limits how many times the program is run, at least 4 and by
	// default 200. Two runs are held back for the result.
	MaxRuns int
	// Progress, when set, gets one line per run.
	Progress io.Writer
}

// ShrinkResult is what Shrink found.
type ShrinkResult struct {
	// Instance is the smallest failing instance found: the failing run with
	// every answer that isn't needed for the failure taken from the passing
	// run, and only the calls the program made in its last run.
	Instance *instance.Instance `json:"-"`
	// Failure is how the failing run ended, such as "exit 1".
	Failure string `json:"failure"`
	// Changed is how many answers differ between the two runs.
	Changed int `json:"changed"`
	// Needed describes the failing run's answers the failure needs.
	Needed []string `json:"needed"`
	// Runs is how many times the program was run.
	Runs int `json:"runs"`
	// Minimal is false when MaxRuns stopped the search early; the answers in
	// Needed then still make it fail, but fewer might do.
	Minimal bool `json:"minimal"`
	// Pruned is true when the instance holds only the calls the program
	// made; false when the pruned instance didn't fail again and the
	// instance keeps every call.
	Pruned bool `json:"pruned"`
	// FailingOnly is how many calls only the failing run made. Their answers
	// can't be swapped, so they stay the failing run's.
	FailingOnly int `json:"failing_only"`
}

// ErrInterrupted means a run was interrupted (Ctrl-C), so its result can't
// be trusted.
var ErrInterrupted = errors.New("interrupted")

// errOutOfRuns stops the search when MaxRuns is reached.
var errOutOfRuns = errors.New("out of runs")

// ErrNotAFailure means the run given as the failing one exited 0, and no
// Match says what its failure looks like.
var ErrNotAFailure = errors.New("the failing run exited 0; give -match to say what its failure looks like")

// ErrFailureDoesNotReplay means the failing run didn't fail again when
// replayed with its own answers, so there is nothing to shrink.
var ErrFailureDoesNotReplay = errors.New("the failure doesn't happen again on replay")

// ErrNotTheAnswers means the program still fails with the passing run's
// answers, so the answers that differ aren't what makes it fail.
var ErrNotTheAnswers = errors.New("the program fails with the passing run's answers too")

// Shrink looks for the fewest of the failing run's answers that still make
// the program fail the same way. It pairs the calls of the two runs as diff
// does, then replays the program again and again with the passing run's
// answer swapped in for some of the answers that differ, narrowing down by
// delta debugging which of the failing answers matter. A run counts as the
// same failure when it ends the same way as the failing run (and, with
// Match, prints the match).
func Shrink(ctx context.Context, pass, fail *instance.Instance, opts ShrinkOptions) (*ShrinkResult, error) {
	for _, inst := range []*instance.Instance{pass, fail} {
		if err := inst.Validate(); err != nil {
			return nil, err
		}
	}
	if Ended(fail.Result) == "exit 0" && opts.Match == nil {
		return nil, ErrNotAFailure
	}
	if opts.MaxRuns == 0 {
		opts.MaxRuns = 200
	}
	if opts.MaxRuns < 4 {
		return nil, fmt.Errorf("the run limit is %d; it has to be at least 4", opts.MaxRuns)
	}
	s := &shrinker{ctx: ctx, pass: pass, fail: fail, opts: opts, cache: map[string]bool{}}
	pair, usedFail := pairCalls(pass.Calls, fail.Calls)
	for i, j := range pair {
		if j >= 0 && answerDiffers(pass.Calls[i], fail.Calls[j]) {
			s.changed = append(s.changed, swap{passIdx: i, failIdx: j})
		}
		if j < 0 {
			s.passOnly = append(s.passOnly, pass.Calls[i])
		}
	}
	sort.Slice(s.changed, func(x, y int) bool { return s.changed[x].failIdx < s.changed[y].failIdx })
	res := &ShrinkResult{Failure: Ended(fail.Result), Changed: len(s.changed), Needed: []string{}}
	for _, used := range usedFail {
		if !used {
			res.FailingOnly++
		}
	}

	all := make([]int, len(s.changed))
	for k := range all {
		all[k] = k
	}
	if ok, err := s.fails(all); err != nil {
		return nil, err
	} else if !ok {
		res.Runs = s.runs
		if s.lastEnded == res.Failure {
			return res, fmt.Errorf("%w: it ended %s, but didn't print the match", ErrFailureDoesNotReplay, s.lastEnded)
		}
		return res, fmt.Errorf("%w: it ended %s, not %s", ErrFailureDoesNotReplay, s.lastEnded, res.Failure)
	}
	if ok, err := s.fails(nil); err != nil {
		return nil, err
	} else if ok {
		res.Runs = s.runs
		if res.FailingOnly > 0 {
			return res, fmt.Errorf("%w (the answers to the %d calls only the failing run made can't be swapped, and stay in)", ErrNotTheAnswers, res.FailingOnly)
		}
		return res, ErrNotTheAnswers
	}

	keep, minimal, err := s.ddmin(all)
	if err != nil {
		return nil, err
	}
	res.Minimal = minimal

	// One more run of the result, to keep what it printed and which calls it
	// made, then those calls only.
	inst := s.build(keep)
	rep, err := s.replay(inst)
	if err != nil {
		return nil, err
	}
	if !s.isFailure(rep) {
		return nil, fmt.Errorf("the program didn't fail again with the %d answers found; it may not fail the same way every time", len(keep))
	}
	inst.Result = rep.Replayed
	inst.Created = time.Now().UTC()
	inst.Tool = "codeinstances " + Version
	if len(s.opts.Argv) > 0 {
		inst.Command.Argv = s.opts.Argv
	}
	pruned := prune(inst, rep.CallsUsed)
	rep2, err := s.replay(pruned)
	if err != nil {
		return nil, err
	}
	if s.isFailure(rep2) {
		pruned.Result = rep2.Replayed
		inst = pruned
		res.Pruned = true
	}
	res.Instance = inst
	res.Runs = s.runs
	for _, k := range keep {
		sw := s.changed[k]
		pc, fc := pass.Calls[sw.passIdx], fail.Calls[sw.failIdx]
		res.Needed = append(res.Needed, fmt.Sprintf("%s %s (failing call %d): %s", fc.Request.Method, fc.Request.URL, fc.Seq, strings.Join(answerDiff(pc, fc), "; ")))
	}
	return res, nil
}

type swap struct{ passIdx, failIdx int }

type shrinker struct {
	ctx        context.Context
	pass, fail *instance.Instance
	opts       ShrinkOptions
	changed    []swap
	passOnly   []instance.Call
	cache      map[string]bool
	runs       int
	lastEnded  string
}

// build makes the instance to replay: the failing run's calls, with the
// passing run's answer for every changed call not in keep, and the passing
// run's unpaired calls after them, in case the program takes the passing
// run's path.
func (s *shrinker) build(keep []int) *instance.Instance {
	kept := map[int]bool{}
	for _, k := range keep {
		kept[k] = true
	}
	calls := make([]instance.Call, 0, len(s.fail.Calls)+len(s.passOnly))
	calls = append(calls, s.fail.Calls...)
	for k, sw := range s.changed {
		if !kept[k] {
			pc := s.pass.Calls[sw.passIdx]
			calls[sw.failIdx].Response, calls[sw.failIdx].Error = pc.Response, pc.Error
		}
	}
	calls = append(calls, s.passOnly...)
	for i := range calls {
		calls[i].Seq = i + 1
	}
	inst := *s.fail
	inst.ID = ""
	inst.Calls = calls
	return &inst
}

func (s *shrinker) replay(inst *instance.Instance) (*Report, error) {
	s.runs++
	rep, err := Replay(s.ctx, inst, ReplayOptions{Argv: s.opts.Argv, Dir: s.opts.Dir, Timeout: s.opts.Timeout})
	if err != nil {
		return nil, err
	}
	if s.ctx.Err() != nil || rep.Replayed.Signal == "interrupt" && s.fail.Result.Signal != "interrupt" {
		return nil, ErrInterrupted
	}
	return rep, nil
}

func (s *shrinker) isFailure(rep *Report) bool {
	s.lastEnded = Ended(rep.Replayed)
	if s.lastEnded != Ended(s.fail.Result) {
		return false
	}
	if s.opts.Match != nil {
		out := append(rep.Replayed.Stdout.MustBytes(), rep.Replayed.Stderr.MustBytes()...)
		return s.opts.Match.Match(out)
	}
	return true
}

// fails replays with the failing answers in keep and says whether the
// program failed the same way.
func (s *shrinker) fails(keep []int) (bool, error) {
	key := subsetKey(keep)
	if v, ok := s.cache[key]; ok {
		return v, nil
	}
	// Two runs are held back for the result.
	if s.runs >= s.opts.MaxRuns-2 {
		return false, errOutOfRuns
	}
	rep, err := s.replay(s.build(keep))
	if err != nil {
		return false, err
	}
	ok := s.isFailure(rep)
	s.cache[key] = ok
	if s.opts.Progress != nil {
		verdict := "passes"
		if ok {
			verdict = "fails"
		}
		fmt.Fprintf(s.opts.Progress, "codeinstances: run %d with %d of %d failing answers: %s (%s)\n", s.runs, len(keep), len(s.changed), verdict, s.lastEnded)
	}
	return ok, nil
}

// ddmin is Zeller's delta debugging minimisation over the changed answers:
// it returns a subset of c that still fails and from which no single answer
// can be removed (when it ran to the end), or the smallest found so far when
// the run limit stops it.
func (s *shrinker) ddmin(c []int) ([]int, bool, error) {
	c, minimal, err := s.ddminRuns(c)
	if errors.Is(err, errOutOfRuns) {
		return c, false, nil
	}
	return c, minimal, err
}

func (s *shrinker) ddminRuns(c []int) ([]int, bool, error) {
	n := 2
	for len(c) >= 2 {
		parts := split(c, n)
		reduced := false
		for _, p := range parts {
			ok, err := s.fails(p)
			if err != nil {
				return c, false, err
			}
			if ok {
				c, n, reduced = p, 2, true
				break
			}
		}
		if !reduced && n > 2 {
			for _, p := range parts {
				comp := without(c, p)
				ok, err := s.fails(comp)
				if err != nil {
					return c, false, err
				}
				if ok {
					c, n, reduced = comp, max(n-1, 2), true
					break
				}
			}
		}
		if !reduced {
			if n >= len(c) {
				break
			}
			n = min(2*n, len(c))
		}
	}
	return c, true, nil
}

func split(c []int, n int) [][]int {
	var parts [][]int
	start := 0
	for i := 0; i < n; i++ {
		end := start + (len(c)-start)/(n-i)
		parts = append(parts, c[start:end])
		start = end
	}
	return parts
}

func without(c, p []int) []int {
	drop := map[int]bool{}
	for _, x := range p {
		drop[x] = true
	}
	var out []int
	for _, x := range c {
		if !drop[x] {
			out = append(out, x)
		}
	}
	return out
}

func subsetKey(keep []int) string {
	sorted := append([]int(nil), keep...)
	sort.Ints(sorted)
	parts := make([]string, len(sorted))
	for i, k := range sorted {
		parts[i] = strconv.Itoa(k)
	}
	return strings.Join(parts, ",")
}

// prune keeps only the calls a replay used, numbered again from 1.
func prune(inst *instance.Instance, used []int) *instance.Instance {
	keep := map[int]bool{}
	for _, seq := range used {
		keep[seq] = true
	}
	out := *inst
	out.Calls = nil
	for _, c := range inst.Calls {
		if keep[c.Seq] {
			c.Seq = len(out.Calls) + 1
			out.Calls = append(out.Calls, c)
		}
	}
	return &out
}

// answerDiffers says whether two paired calls got different answers.
func answerDiffers(a, b instance.Call) bool {
	return len(answerDiff(a, b)) > 0
}

// answerDiff is callDiff without the request body: how the answers differ.
func answerDiff(a, b instance.Call) []string {
	var d []string
	for _, msg := range callDiff(a, b) {
		if !strings.HasPrefix(msg, "request body") {
			d = append(d, msg)
		}
	}
	return d
}
