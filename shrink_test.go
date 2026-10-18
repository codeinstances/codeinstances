// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package codeinstances

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/codeinstances/codeinstances/instance"
)

// sixCalls is a recording of the "shrinkable" child, made by hand: the
// answer to /i is answers[i-1].
func sixCalls(t *testing.T, answers []string, extra ...string) *instance.Instance {
	t.Helper()
	inst := &instance.Instance{Format: instance.Format, Tool: "test",
		Command: instance.Command{Argv: []string{os.Args[0], "-test.run=^$"}}}
	add := func(path, body string) {
		inst.Calls = append(inst.Calls, instance.Call{Seq: len(inst.Calls) + 1, Kind: instance.KindHTTP,
			Request:  instance.Request{Method: "GET", URL: "https://secure.test/" + path},
			Response: &instance.Response{Status: 200, Body: instance.NewBody([]byte(body))}})
	}
	for i, a := range answers {
		add(fmt.Sprint(i+1), a)
	}
	for _, p := range extra {
		add(p, "noted")
	}
	return inst
}

func recordedResult(t *testing.T, inst *instance.Instance) *instance.Instance {
	t.Helper()
	t.Setenv("CI_TEST_CHILD", "shrinkable")
	rep, err := Replay(context.Background(), inst, ReplayOptions{Timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	inst.Result = rep.Replayed
	return inst
}

func TestShrinkFindsTheAnswersThatMatter(t *testing.T) {
	pass := recordedResult(t, sixCalls(t, []string{"good", "good", "good", "good", "good", "good"}))
	fail := recordedResult(t, sixCalls(t, []string{"bad", "good", "bad", "bad", "bad", "bad"}, "report"))
	if pass.Result.ExitCode != 0 || fail.Result.ExitCode != 1 {
		t.Fatalf("pass %d fail %d", pass.Result.ExitCode, fail.Result.ExitCode)
	}
	var progress bytes.Buffer
	res, err := Shrink(context.Background(), pass, fail, ShrinkOptions{Timeout: time.Minute, Progress: &progress})
	if err != nil {
		t.Fatalf("%v\n%s", err, progress.String())
	}
	if res.Changed != 5 || len(res.Needed) != 2 || !res.Minimal || !res.Pruned || res.FailingOnly != 1 || res.Failure != "exit 1" {
		t.Fatalf("%+v\n%s", res, progress.String())
	}
	for i, want := range []string{"https://secure.test/3 (failing call 3)", "https://secure.test/5 (failing call 5)"} {
		if !strings.Contains(res.Needed[i], want) || !strings.Contains(res.Needed[i], `a "good", b "bad"`) {
			t.Errorf("needed %d: %s", i, res.Needed[i])
		}
	}
	if res.Runs > 20 || !strings.Contains(progress.String(), "run 1 with 5 of 5 failing answers: fails (exit 1)") {
		t.Fatalf("%d runs:\n%s", res.Runs, progress.String())
	}

	// The shrunk instance holds only the calls made, with only /3 and /5
	// still bad, and fails on replay the same way.
	inst := res.Instance
	var bodies []string
	for _, c := range inst.Calls {
		bodies = append(bodies, strings.TrimPrefix(c.Request.URL, "https://secure.test/")+"="+string(c.Response.Body.MustBytes()))
	}
	want := []string{"1=good", "2=good", "3=bad", "4=good", "5=bad", "6=good", "report=noted"}
	if !reflect.DeepEqual(bodies, want) {
		t.Fatalf("%v", bodies)
	}
	if inst.Result.ExitCode != 1 || inst.Tool != "codeinstances "+Version {
		t.Fatalf("%+v", inst.Result)
	}
	path := t.TempDir() + "/shrunk.instance"
	if err := instance.Save(path, inst); err != nil {
		t.Fatal(err)
	}
	loaded, _ := instance.Load(path)
	if rep := replayAs(t, loaded, "shrinkable", ReplayOptions{}); !rep.Matched {
		t.Fatal(rep.Divergences)
	}
}

func TestShrinkRefusesWhatItCannotShrink(t *testing.T) {
	pass := recordedResult(t, sixCalls(t, []string{"good", "good", "good", "good", "good", "good"}))
	fail := recordedResult(t, sixCalls(t, []string{"good", "good", "bad", "good", "bad", "good"}, "report"))

	// The failure was recorded with exit 3, but replays as exit 1.
	odd := *fail
	odd.Result.ExitCode = 3
	if _, err := Shrink(context.Background(), pass, &odd, ShrinkOptions{Timeout: time.Minute}); !errors.Is(err, ErrFailureDoesNotReplay) || !strings.Contains(err.Error(), "ended exit 1, not exit 3") {
		t.Fatal(err)
	}
	// A failing "passing" run means the answers aren't the cause.
	if _, err := Shrink(context.Background(), fail, fail, ShrinkOptions{Timeout: time.Minute}); !errors.Is(err, ErrNotTheAnswers) {
		t.Fatal(err)
	}
	// When the cause is in a call only the failing run made, it says so.
	short := recordedResult(t, sixCalls(t, []string{"good", "good"}))
	if _, err := Shrink(context.Background(), short, fail, ShrinkOptions{Timeout: time.Minute}); !errors.Is(err, ErrNotTheAnswers) || !strings.Contains(err.Error(), "the 5 calls only the failing run made") {
		t.Fatal(err)
	}
	// A run that exited 0 isn't a failure unless Match says what is.
	if _, err := Shrink(context.Background(), fail, pass, ShrinkOptions{Timeout: time.Minute}); !errors.Is(err, ErrNotAFailure) {
		t.Fatal(err)
	}
	// With -match, the output has to say so too.
	res, err := Shrink(context.Background(), pass, fail, ShrinkOptions{Timeout: time.Minute, Match: regexp.MustCompile("both bad")})
	if err != nil || len(res.Needed) != 2 {
		t.Fatal(err)
	}
	if _, err := Shrink(context.Background(), pass, fail, ShrinkOptions{Timeout: time.Minute, Match: regexp.MustCompile("no such text")}); !errors.Is(err, ErrFailureDoesNotReplay) || !strings.Contains(err.Error(), "didn't print the match") {
		t.Fatal(err)
	}
}

func TestShrinkStopsAtTheRunLimit(t *testing.T) {
	pass := recordedResult(t, sixCalls(t, []string{"good", "good", "good", "good", "good", "good"}))
	fail := recordedResult(t, sixCalls(t, []string{"bad", "bad", "bad", "bad", "bad", "bad"}, "report"))
	for _, max := range []int{4, 5, 7} {
		res, err := Shrink(context.Background(), pass, fail, ShrinkOptions{Timeout: time.Minute, MaxRuns: max})
		if err != nil {
			t.Fatal(err)
		}
		if res.Minimal || len(res.Needed) < 2 || res.Runs > max {
			t.Fatalf("max %d: %+v", max, res)
		}
	}
	if _, err := Shrink(context.Background(), pass, fail, ShrinkOptions{MaxRuns: 3}); err == nil || !strings.Contains(err.Error(), "at least 4") {
		t.Fatal(err)
	}
}

func TestDdminHelpers(t *testing.T) {
	if got := split([]int{0, 1, 2, 3, 4}, 2); !reflect.DeepEqual(got, [][]int{{0, 1}, {2, 3, 4}}) {
		t.Fatal(got)
	}
	if got := split([]int{0, 1, 2}, 3); !reflect.DeepEqual(got, [][]int{{0}, {1}, {2}}) {
		t.Fatal(got)
	}
	if got := without([]int{0, 1, 2, 3}, []int{1, 3}); !reflect.DeepEqual(got, []int{0, 2}) {
		t.Fatal(got)
	}
	if subsetKey([]int{3, 1, 2}) != "1,2,3" || subsetKey(nil) != "" {
		t.Fatal("key")
	}
}

func TestInstancesAsRegressionTests(t *testing.T) {
	pass := recordedResult(t, sixCalls(t, []string{"good", "good", "good", "good", "good", "good"}))
	fail := recordedResult(t, sixCalls(t, []string{"good", "good", "bad", "good", "bad", "good"}, "report"))
	t.Setenv("CI_TEST_CHILD", "shrinkable")
	ctx := context.Background()

	// A passing run must replay exactly.
	r, err := TestInstance(ctx, pass, TestOptions{Timeout: time.Minute})
	if err != nil || !r.Passed || r.Expect != ExpectMatch {
		t.Fatalf("%+v %v", r, err)
	}
	// A failing run must exit 0 now. The program still fails, so the test does.
	r, err = TestInstance(ctx, fail, TestOptions{Timeout: time.Minute})
	if err != nil || r.Passed || r.Expect != ExpectPass || r.Problems[0] != "ended exit 1, want exit 0 (recorded exit 1)" {
		t.Fatalf("%+v %v", r, err)
	}
	// The fixed program (here, the passing child's behaviour stands in for
	// one that handles the bad answers) passes the same test.
	t.Setenv("CI_TEST_CHILD", "web")
	fixed := *fail
	fixed.Command.Argv = []string{os.Args[0], "-test.run=^$"}
	r, err = TestInstance(ctx, &fixed, TestOptions{Timeout: time.Minute})
	if err != nil || !r.Passed || len(r.Notes) == 0 {
		t.Fatalf("%+v %v", r, err)
	}
	// Expectations can be set by hand.
	t.Setenv("CI_TEST_CHILD", "shrinkable")
	if r, _ := TestInstance(ctx, fail, TestOptions{Expect: ExpectMatch, Timeout: time.Minute}); !r.Passed {
		t.Fatalf("%+v", r)
	}
	if _, err := TestInstance(ctx, fail, TestOptions{Expect: "maybe"}); err == nil {
		t.Fatal("unknown expectation accepted")
	}
}

func TestShrinkStopsWhenCancelled(t *testing.T) {
	pass := recordedResult(t, sixCalls(t, []string{"good", "good", "good", "good", "good", "good"}))
	fail := recordedResult(t, sixCalls(t, []string{"bad", "good", "bad", "bad", "bad", "bad"}, "report"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Shrink(ctx, pass, fail, ShrinkOptions{Timeout: time.Minute}); err == nil {
		t.Fatal("a cancelled shrink went on")
	}
	t.Setenv("CI_TEST_CHILD", "shrinkable")
	if _, err := TestInstance(ctx, pass, TestOptions{Timeout: time.Minute}); err == nil {
		t.Fatal("a cancelled test passed")
	}
}
