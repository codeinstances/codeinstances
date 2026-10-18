// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package codeinstances

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/codeinstances/codeinstances/instance"
	"github.com/codeinstances/codeinstances/internal/ca"
	"github.com/codeinstances/codeinstances/internal/capture"
)

// ForkOptions says where to fork an instance and how to run on.
type ForkOptions struct {
	// At is the seq of the parent's first call not to answer from the
	// parent. The calls before it are answered as recorded; from it on,
	// requests go out to the real servers (or, with Offline, get a 502).
	// At can be one past the last call, to answer every recorded call and
	// send only what the program asks for beyond them.
	At int
	// Answer, when set, is the answer to give the request that matches the
	// parent's call At, in place of sending it: a what-if.
	Answer *Answer
	// Offline sends nothing: requests the parent has no answer for get a
	// 502 and are kept as failed calls.
	Offline bool
	// FreshValues hands the program new clock and random values. By default
	// the parent's values come back in order until they run out, then fresh
	// ones.
	FreshValues bool
	// Argv runs a different command, such as a changed build. Empty means
	// the parent's command.
	Argv []string
	Dir  string
	// The rest are as for Record.
	Resolve     map[string]string
	UpstreamCAs [][]byte
	KeepSecrets bool
	Timeout     time.Duration
	Stdout      io.Writer
	Stderr      io.Writer
}

// Answer is a made-up answer for a fork: a status, headers and a body, or
// an error for a request that got no answer.
type Answer struct {
	Status int                 `json:"status"`
	Header map[string][]string `json:"header,omitempty"`
	Body   string              `json:"body,omitempty"`
	Error  string              `json:"error,omitempty"`
}

// ReadAnswer reads an answer from a JSON file such as
// {"status": 503, "header": {"Retry-After": ["30"]}, "body": "busy"}.
func ReadAnswer(path string) (*Answer, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var a Answer
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if (a.Error == "") == (a.Status == 0) {
		return nil, fmt.Errorf("%s: give a status (100 to 999) or an error, not both", path)
	}
	if a.Error == "" && (a.Status < 100 || a.Status > 999) {
		return nil, fmt.Errorf("%s: status %d", path, a.Status)
	}
	return &a, nil
}

// ForkSummary counts where a fork's calls got their answers.
type ForkSummary struct {
	FromParent int `json:"from_parent"`
	Given      int `json:"given"`
	Sent       int `json:"sent"`
	Offline    int `json:"offline"`
}

// Summarize counts a fork's calls by where their answers came from.
func Summarize(inst *instance.Instance) ForkSummary {
	var s ForkSummary
	for _, c := range inst.Calls {
		switch {
		case c.FromSeq > 0:
			s.FromParent++
		case c.Given:
			s.Given++
		case c.Error == capture.ErrOffline:
			s.Offline++
		default:
			s.Sent++
		}
	}
	return s
}

// Fork runs the parent instance's program again with the parent's answers
// for the calls before step At, and from there on runs live: requests go out
// to the real servers and are kept, as when recording. The result is a new
// instance, the fork, that says which parent and step it came from, and for
// each call whether its answer came from the parent, was given, or was sent.
// Compare it with the parent to see what changed from that step on.
func Fork(ctx context.Context, parent *instance.Instance, opts ForkOptions) (*instance.Instance, error) {
	if err := parent.Validate(); err != nil {
		return nil, err
	}
	if opts.At < 1 || opts.At > len(parent.Calls)+1 {
		return nil, fmt.Errorf("the parent has calls 1 to %d; fork at one of them, or at %d to answer them all", len(parent.Calls), len(parent.Calls)+1)
	}
	if opts.Answer != nil && opts.At > len(parent.Calls) {
		return nil, errors.New("an answer replaces the parent's call at the step, and there is none there")
	}
	var answers []capture.Answer
	for _, c := range parent.Calls[:opts.At-1] {
		answers = append(answers, capture.Answer{Call: c})
	}
	if a := opts.Answer; a != nil {
		c := parent.Calls[opts.At-1]
		given := instance.Call{Seq: c.Seq, Kind: c.Kind, Request: c.Request, Error: a.Error}
		if a.Error == "" {
			given.Response = &instance.Response{Status: a.Status, Header: a.Header, Body: instance.NewBody([]byte(a.Body))}
		}
		answers = append(answers, capture.Answer{Call: given, Given: true})
	}
	argv := opts.Argv
	if len(argv) == 0 {
		argv = parent.Command.Argv
	}

	auth, err := ca.New("CodeInstances fork")
	if err != nil {
		return nil, err
	}
	upstream, err := upstreamTLS(opts.UpstreamCAs)
	if err != nil {
		return nil, err
	}
	proxy, err := capture.StartRecord(capture.RecordConfig{Authority: auth, Resolve: opts.Resolve, UpstreamTLS: upstream,
		KeepSecrets: opts.KeepSecrets, Answers: answers, Offline: opts.Offline})
	if err != nil {
		return nil, err
	}
	defer proxy.Close()
	work, err := os.MkdirTemp("", "codeinstances-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	mode := "record"
	if !opts.FreshValues {
		mode = "replay"
		if err := writeValues(filepath.Join(work, "values-in"), parent.Values); err != nil {
			return nil, err
		}
	}
	dir := opts.Dir
	if dir == "" {
		dir, _ = os.Getwd()
	}
	created := time.Now().UTC()
	res, err := run(ctx, runSpec{
		argv: argv, dir: opts.Dir, work: work, mode: mode, proxy: proxy.URL(), caPEM: auth.CertPEM(),
		env: parent.Command.Env, timeout: opts.Timeout, stdout: opts.Stdout, stderr: opts.Stderr,
	})
	if err != nil {
		return nil, err
	}
	proxy.Close()
	values, err := readValues(filepath.Join(work, "values-out"))
	if err != nil {
		return nil, err
	}
	fork := &instance.Instance{
		Format:  instance.Format,
		Created: created,
		Tool:    "codeinstances " + Version,
		Command: instance.Command{Argv: argv, Dir: dir, Env: parent.Command.Env},
		Result:  res,
		Calls:   proxy.Calls(),
		Fork:    &instance.Fork{Parent: parent.ID, At: opts.At},
	}
	for i, l := range values {
		fork.Values = append(fork.Values, instance.Value{Seq: i + 1, Kind: l.Kind, Value: l.Value})
	}
	return fork, nil
}
