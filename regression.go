// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package codeinstances

import (
	"context"
	"fmt"
	"time"

	"github.com/codeinstances/codeinstances/instance"
)

// What a regression test expects of a replay.
const (
	// ExpectAuto expects ExpectMatch of a run that passed when it was
	// recorded (exit 0) and ExpectPass of one that didn't.
	ExpectAuto = "auto"
	// ExpectMatch expects the replay to match the recording exactly.
	ExpectMatch = "match"
	// ExpectPass expects the program to exit 0 against the recorded
	// answers: the failure it recorded is fixed and stays fixed.
	ExpectPass = "pass"
)

// TestOptions says how to run instances as regression tests.
type TestOptions struct {
	// Argv runs a different command against each instance, such as the
	// build under test. Empty means each instance's recorded command.
	Argv []string
	Dir  string
	// Expect is ExpectAuto (the default), ExpectMatch or ExpectPass.
	Expect       string
	IgnoreStderr bool
	// Timeout limits each replay; zero means no limit.
	Timeout time.Duration
}

// TestResult is the outcome of running one instance as a test.
type TestResult struct {
	Expect   string `json:"expect"`
	Passed   bool   `json:"passed"`
	Recorded string `json:"recorded"`
	Replayed string `json:"replayed"`
	// Problems say why the test failed.
	Problems []string `json:"problems"`
	// Notes are differences that didn't fail the test, such as requests
	// a fixed program makes that the failing run didn't.
	Notes      []string `json:"notes"`
	DurationMS int64    `json:"duration_ms"`
}

// TestInstance replays an instance as a regression test. A run that passed
// when it was recorded has to replay exactly, so a change that alters what
// the program sends or prints is caught. A run that failed when it was
// recorded has to exit 0 now, against the same answers that once broke it.
func TestInstance(ctx context.Context, inst *instance.Instance, opts TestOptions) (*TestResult, error) {
	expect := opts.Expect
	switch expect {
	case "", ExpectAuto:
		expect = ExpectMatch
		if Ended(inst.Result) != "exit 0" {
			expect = ExpectPass
		}
	case ExpectMatch, ExpectPass:
	default:
		return nil, fmt.Errorf("unknown expectation %q (want auto, match or pass)", opts.Expect)
	}
	start := time.Now()
	rep, err := Replay(ctx, inst, ReplayOptions{Argv: opts.Argv, Dir: opts.Dir, IgnoreStderr: opts.IgnoreStderr, Timeout: opts.Timeout})
	if err != nil {
		return nil, err
	}
	if ctx.Err() != nil || rep.Replayed.Signal == "interrupt" && inst.Result.Signal != "interrupt" {
		return nil, ErrInterrupted
	}
	r := &TestResult{Expect: expect, Recorded: Ended(inst.Result), Replayed: Ended(rep.Replayed),
		Problems: []string{}, Notes: []string{}, DurationMS: time.Since(start).Milliseconds()}
	if expect == ExpectMatch {
		r.Problems = append(r.Problems, rep.Divergences...)
	} else {
		if r.Replayed != "exit 0" {
			r.Problems = append(r.Problems, fmt.Sprintf("ended %s, want exit 0 (recorded %s)", r.Replayed, r.Recorded))
		}
		r.Notes = append(r.Notes, rep.Divergences...)
	}
	r.Passed = len(r.Problems) == 0
	return r, nil
}
