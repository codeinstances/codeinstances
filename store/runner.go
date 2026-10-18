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
	"os"
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
	DefaultMaxKept    = 1000
	// MaxRunOutput is how much of each output stream a run's answer carries:
	// the last part, which is where errors are.
	MaxRunOutput  = 64 << 10
	maxRunRequest = 1 << 20
)

// Runner lets a Server run the instances it keeps, on demand: replay one,
// or fork one at a step. Every run is answered from the recording: requests
// through the proxy settings never reach the network. It runs only
// instances signed by a key in Trusted, and only the command each instance
// recorded.
//
// A trusted key here means more than provenance: it says the runs it signs
// are fit to run on this machine, again, with the answers they recorded.
// With Answers set, a token holder also chooses the answer at a fork's step,
// which is input the program acts on (a run that downloads a script and runs
// it would run whatever answer it was given), so give runs a user and a
// machine of their own.
type Runner struct {
	// Trusted are the keys whose instances the server runs. Keep them apart
	// from the keys whose uploads it takes: signed is not the same as safe to
	// run. A run signed only by SignKey is never run.
	Trusted []codeinstances.TrustedKey
	// Answers lets fork requests give an answer at the step. Without it, a
	// fork gets the parent's answers up to the step, and 502s after.
	Answers bool
	// Dir is the working directory for runs; empty means a fresh empty
	// folder for each run, removed afterwards.
	Dir string
	// Timeout limits each run; zero means DefaultRunTimeout.
	Timeout time.Duration
	// MaxRuns limits runs at once; zero means DefaultMaxRuns. More get 503.
	MaxRuns int
	// SignKey, when set, signs the forks the server keeps, as Signer: a
	// signature that says "ran on this store, with these answers", so its
	// key mustn't be among the Trusted keys or the store's upload keys.
	SignKey ed25519.PrivateKey
	Signer  string
	// MaxKept limits how many forks the store keeps; zero means
	// DefaultMaxKept. Forks are kept only when a request asks.
	MaxKept int
	// Log, when set, gets the details of runs that failed, which the
	// answer leaves out.
	Log func(format string, args ...any)

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
// always offline. Answer needs a store that takes answers; Keep asks the
// store to keep the fork, so it can be downloaded.
type ForkRequest struct {
	At          int                   `json:"at"`
	Answer      *codeinstances.Answer `json:"answer,omitempty"`
	FreshValues bool                  `json:"fresh_values,omitempty"`
	Keep        bool                  `json:"keep,omitempty"`
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
	// Fork is the id of the fork the server kept, and Path where to get it;
	// both are empty when the request didn't ask to keep it.
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

func (rn *Runner) logf(format string, args ...any) {
	if rn.Log != nil {
		rn.Log(format, args...)
	}
}

// trustedRun says whether a key in rn.Trusted, other than the runner's own
// signing key, signed the instance.
func (rn *Runner) trustedRun(inst *instance.Instance) bool {
	checks, err := codeinstances.CheckSignatures(inst, rn.Trusted)
	if err != nil || len(rn.Trusted) == 0 {
		return false
	}
	for _, c := range checks {
		if c.Trusted == nil {
			continue
		}
		if rn.SignKey != nil && c.Trusted.Key.Equal(rn.SignKey.Public()) {
			continue
		}
		return true
	}
	return false
}

// keptForks counts the forks in the store.
func (s *Server) keptForks() int {
	n := 0
	for _, e := range s.Store.List() {
		if e.ForkOf != "" {
			n++
		}
	}
	return n
}

// run handles POST /v1/instances/{ref}/{replay|fork}.
func (s *Server) run(w http.ResponseWriter, r *http.Request, ref, kind string) {
	rn := s.Runner
	if s.Token == "" {
		// Without a token anyone who reaches the port could run instances.
		problem(w, http.StatusForbidden, "a store runs instances only with a token")
		return
	}
	// The slot comes first, so requests waiting on one don't each hold a
	// parsed instance in memory.
	select {
	case rn.slot() <- struct{}{}:
		defer func() { <-rn.slot() }()
	default:
		w.Header().Set("Retry-After", "10")
		problem(w, http.StatusServiceUnavailable, "the store is running as many instances as it may; try again")
		return
	}
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
	if !rn.trustedRun(inst) {
		problem(w, http.StatusForbidden, "the store runs only instances signed by a key it trusts to run")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRunRequest))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			problem(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the request is over %d bytes", maxRunRequest))
			return
		}
		problem(w, http.StatusBadRequest, "the request was cut short")
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
	if forkReq.Answer != nil && !rn.Answers {
		problem(w, http.StatusForbidden, "this store doesn't take given answers (it was started without -run-answers)")
		return
	}
	if kind == "fork" {
		if err := codeinstances.CheckForkStep(inst, forkReq.At, forkReq.Answer != nil); err != nil {
			problem(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
	}
	if forkReq.Keep && s.keptForks() >= rn.maxKept() {
		problem(w, http.StatusInsufficientStorage, "the store keeps as many forks as it may; remove some first")
		return
	}

	dir := rn.Dir
	if dir == "" {
		if dir, err = os.MkdirTemp("", "codeinstances-run-"); err != nil {
			problem(w, http.StatusInternalServerError, "the store couldn't make a folder for the run")
			return
		}
		defer os.RemoveAll(dir)
	}
	// A run stops when the client goes away, as well as at the time limit.
	ctx, cancel := context.WithTimeout(r.Context(), rn.timeout())
	defer cancel()
	res := &RunResult{Instance: id, Kind: kind}
	env := codeinstances.MinimalEnv()
	if kind == "replay" {
		rep, err := codeinstances.Replay(ctx, inst, codeinstances.ReplayOptions{Dir: dir, Strict: replayReq.Strict,
			IgnoreStderr: replayReq.IgnoreStderr, Timeout: rn.timeout(), BaseEnv: env, Isolate: true})
		if err != nil {
			rn.logf("replay of %s: %v", id, err)
			problem(w, http.StatusUnprocessableEntity, "the instance couldn't be run here (the store's log has the details)")
			return
		}
		res.Matched, res.Divergences = rep.Matched, rep.Divergences
		res.setResult(rep.Replayed)
		writeJSON(w, http.StatusOK, res)
		return
	}
	f, err := codeinstances.Fork(ctx, inst, codeinstances.ForkOptions{At: forkReq.At, Answer: forkReq.Answer, Offline: true,
		FreshValues: forkReq.FreshValues, Dir: dir, Timeout: rn.timeout(), BaseEnv: env, Isolate: true})
	if err != nil {
		rn.logf("fork of %s: %v", id, err)
		problem(w, http.StatusUnprocessableEntity, "the fork couldn't be run here (the store's log has the details)")
		return
	}
	// The fork names the parent's folder, not the store's.
	f.Command.Dir = inst.Command.Dir
	sum := codeinstances.Summarize(f)
	res.Summary = &sum
	res.setResult(f.Result)
	if forkReq.Keep {
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
		if max := s.maxBytes(); int64(len(fdata)) > max {
			problem(w, http.StatusInsufficientStorage, fmt.Sprintf("the fork is over %d bytes, so it wasn't kept", max))
			return
		}
		fid, _, err := s.Store.Put(fdata)
		if err != nil {
			problem(w, http.StatusInternalServerError, "the fork couldn't be kept")
			return
		}
		res.Fork, res.Path = fid, "/v1/instances/"+fid
	}
	writeJSON(w, http.StatusOK, res)
}

func (rn *Runner) maxKept() int {
	if rn.MaxKept > 0 {
		return rn.MaxKept
	}
	return DefaultMaxKept
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
