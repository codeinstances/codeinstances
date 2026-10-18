// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/codeinstances/codeinstances"
	"github.com/codeinstances/codeinstances/instance"
)

// Defaults for running instances on demand.
const (
	DefaultRunTimeout = time.Minute
	DefaultMaxRuns    = 1
	// MaxRunOutput is how much of each output stream a run's answer carries:
	// the last part, which is where errors are.
	MaxRunOutput  = 64 << 10
	maxRunRequest = 1 << 20
)

// Runner lets a Server run the instances it keeps, on demand: replay one
// offline, or fork one offline at a step with an answer given there. It
// runs only instances signed by a key in Server.Trusted, and only the
// command each instance recorded, so what can run is what a trusted key
// vouched for. Nothing a run does reaches the network.
type Runner struct {
	// Dir is the working directory for runs; empty means the server's own.
	Dir string
	// Timeout limits each run; zero means DefaultRunTimeout.
	Timeout time.Duration
	// MaxRuns limits runs at once; zero means DefaultMaxRuns. More get 503.
	MaxRuns int
	// SignKey, when set, signs the forks the server keeps, as Signer.
	SignKey ed25519.PrivateKey
	Signer  string

	once  sync.Once
	slots chan struct{}
}

func (rn *Runner) slot() chan struct{} {
	rn.once.Do(func() {
		n := rn.MaxRuns
		if n <= 0 {
			n = DefaultMaxRuns
		}
		rn.slots = make(chan struct{}, n)
	})
	return rn.slots
}

func (rn *Runner) timeout() time.Duration {
	if rn.Timeout > 0 {
		return rn.Timeout
	}
	return DefaultRunTimeout
}

// ReplayRequest is the body of POST /v1/instances/{id}/replay. Every field
// is optional.
type ReplayRequest struct {
	Strict       bool `json:"strict,omitempty"`
	IgnoreStderr bool `json:"ignore_stderr,omitempty"`
}

// ForkRequest is the body of POST /v1/instances/{id}/fork. The fork is
// always offline.
type ForkRequest struct {
	At          int                   `json:"at"`
	Answer      *codeinstances.Answer `json:"answer,omitempty"`
	FreshValues bool                  `json:"fresh_values,omitempty"`
}

// RunResult is a server's answer to a run.
type RunResult struct {
	// Instance is the id of the instance that was run.
	Instance string `json:"instance"`
	// Kind is "replay" or "fork".
	Kind string `json:"kind"`
	// Matched and Divergences are set for a replay.
	Matched     bool     `json:"matched,omitempty"`
	Divergences []string `json:"divergences,omitempty"`
	// Fork is the id of the fork the server kept, and Path where to get it.
	Fork    string                     `json:"fork,omitempty"`
	Path    string                     `json:"path,omitempty"`
	Summary *codeinstances.ForkSummary `json:"summary,omitempty"`
	// How the run ended.
	ExitCode   int    `json:"exit_code"`
	Signal     string `json:"signal,omitempty"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	// Stdout and Stderr are the last MaxRunOutput bytes of each, as text
	// (bytes that aren't UTF-8 are replaced); Cut says the start is missing.
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Cut    bool   `json:"cut,omitempty"`
}

// tail is the last MaxRunOutput bytes of a body, as text.
func tail(b *instance.Body) (string, bool) {
	data := b.MustBytes()
	cut := false
	if len(data) > MaxRunOutput {
		data, cut = data[len(data)-MaxRunOutput:], true
		// Start at a character, not in the middle of one.
		for len(data) > 0 && !utf8.RuneStart(data[0]) {
			data = data[1:]
		}
	}
	return strings.ToValidUTF8(string(data), "�"), cut
}

func (res *RunResult) setResult(r instance.Result) {
	res.ExitCode, res.Signal, res.TimedOut, res.DurationMS = r.ExitCode, r.Signal, r.TimedOut, r.DurationMS
	var c1, c2 bool
	res.Stdout, c1 = tail(r.Stdout)
	res.Stderr, c2 = tail(r.Stderr)
	res.Cut = c1 || c2 || r.StdoutTruncated || r.StderrTruncated
}

// run handles POST /v1/instances/{ref}/{replay|fork}.
func (s *Server) run(w http.ResponseWriter, r *http.Request, ref, kind string) {
	rn := s.Runner
	data, id, err := s.Store.Get(ref)
	switch {
	case errors.Is(err, ErrBadRef):
		problem(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, ErrNotFound):
		problem(w, http.StatusNotFound, err.Error())
		return
	case errors.Is(err, ErrAmbiguous):
		problem(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		problem(w, http.StatusInternalServerError, "the stored file was changed or removed")
		return
	}
	inst, err := instance.Parse(data)
	if err != nil {
		problem(w, http.StatusInternalServerError, "the stored file doesn't check out")
		return
	}
	// Checked at every run, not only on upload: files can be in the folder
	// from before, or from a store that trusted other keys.
	if _, err := codeinstances.CheckSignatures(inst, s.Trusted); err != nil || len(s.Trusted) == 0 {
		problem(w, http.StatusForbidden, "the store runs only instances signed by a key it trusts")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRunRequest))
	if err != nil {
		problem(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the request is over %d bytes", maxRunRequest))
		return
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		body = []byte("{}")
	}
	var replayReq ReplayRequest
	var forkReq ForkRequest
	if kind == "replay" {
		err = strictJSON(body, &replayReq)
	} else {
		err = strictJSON(body, &forkReq)
		if err == nil && forkReq.Answer != nil {
			// The answer goes through the same checks as an -answer file.
			raw, _ := json.Marshal(forkReq.Answer)
			forkReq.Answer, err = codeinstances.ParseAnswer(raw)
		}
	}
	if err != nil {
		problem(w, http.StatusBadRequest, "the request: "+err.Error())
		return
	}

	select {
	case rn.slot() <- struct{}{}:
		defer func() { <-rn.slot() }()
	default:
		w.Header().Set("Retry-After", "10")
		problem(w, http.StatusServiceUnavailable, "the store is running as many instances as it may; try again")
		return
	}
	// A run stops when the client goes away, as well as at the time limit.
	ctx, cancel := context.WithTimeout(r.Context(), rn.timeout())
	defer cancel()
	res := &RunResult{Instance: id, Kind: kind}
	env := codeinstances.MinimalEnv()
	if kind == "replay" {
		rep, err := codeinstances.Replay(ctx, inst, codeinstances.ReplayOptions{Dir: rn.Dir, Strict: replayReq.Strict,
			IgnoreStderr: replayReq.IgnoreStderr, Timeout: rn.timeout(), BaseEnv: env})
		if err != nil {
			problem(w, http.StatusUnprocessableEntity, "the instance couldn't be run here: "+codeinstances.SafeText(err.Error()))
			return
		}
		res.Matched, res.Divergences = rep.Matched, rep.Divergences
		res.setResult(rep.Replayed)
		writeJSON(w, http.StatusOK, res)
		return
	}
	f, err := codeinstances.Fork(ctx, inst, codeinstances.ForkOptions{At: forkReq.At, Answer: forkReq.Answer, Offline: true,
		FreshValues: forkReq.FreshValues, Dir: rn.Dir, Timeout: rn.timeout(), BaseEnv: env})
	if err != nil {
		problem(w, http.StatusUnprocessableEntity, "the fork couldn't be run here: "+codeinstances.SafeText(err.Error()))
		return
	}
	if rn.SignKey != nil {
		if err := f.Sign(rn.SignKey, rn.Signer, time.Now()); err != nil {
			problem(w, http.StatusInternalServerError, "the fork couldn't be signed")
			return
		}
	}
	fdata, err := instance.Marshal(f)
	if err != nil {
		problem(w, http.StatusInternalServerError, "the fork couldn't be kept")
		return
	}
	fid, _, err := s.Store.Put(fdata)
	if err != nil {
		problem(w, http.StatusInternalServerError, "the fork couldn't be kept")
		return
	}
	sum := codeinstances.Summarize(f)
	res.Fork, res.Path, res.Summary = fid, "/v1/instances/"+fid, &sum
	res.setResult(f.Result)
	writeJSON(w, http.StatusOK, res)
}

// strictJSON decodes one JSON object, refusing unknown members and
// anything after it.
func strictJSON(data []byte, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("more than one JSON value")
	}
	return nil
}
