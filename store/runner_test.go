// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codeinstances/codeinstances"
	"github.com/codeinstances/codeinstances/hook"
	"github.com/codeinstances/codeinstances/instance"
)

func TestMain(m *testing.M) {
	switch os.Getenv("STORE_TEST_CHILD") {
	case "":
		os.Exit(m.Run())
	case "fetch":
		// Fetches one page and fails when the answer isn't a 200.
		resp, err := hook.Client().Get("http://plain.test/x")
		if err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(2)
		}
		b, _ := io.ReadAll(resp.Body)
		fmt.Println(resp.StatusCode, string(b), "secret="+os.Getenv("STORE_TEST_SECRET"))
		if resp.StatusCode != 200 {
			fmt.Fprintln(os.Stderr, "error: the page said", resp.StatusCode)
			os.Exit(1)
		}
		os.Exit(0)
	case "sleep":
		time.Sleep(1500 * time.Millisecond)
		fmt.Println("slept")
		os.Exit(0)
	}
	os.Exit(99)
}

type runFixture struct {
	srv     *Server
	key     ed25519.PrivateKey
	signed  string // id of a signed recording of the fetch child
	sleeper string // id of a signed recording of the sleep child
	plain   string // id of the same fetch run, unsigned
}

func newRunFixture(t *testing.T) *runFixture {
	t.Helper()
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "fine") }))
	defer page.Close()
	key, _ := codeinstances.GenerateKey()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &runFixture{key: key, srv: &Server{Store: st, Token: "sesame",
		Trusted: []codeinstances.TrustedKey{{Key: key.Public().(ed25519.PublicKey), Label: "ci"}},
		Runner:  &Runner{Timeout: time.Minute}}}
	record := func(child string, sign bool) string {
		t.Setenv("STORE_TEST_CHILD", child)
		inst, err := codeinstances.Record(context.Background(), codeinstances.RecordOptions{
			Argv: []string{os.Args[0], "-test.run=^$"}, KeepEnv: []string{"STORE_TEST_CHILD"},
			Resolve: map[string]string{"plain.test": strings.TrimPrefix(page.URL, "http://")}})
		if err != nil {
			t.Fatal(err)
		}
		if sign {
			inst.Sign(key, "ci", time.Now())
		}
		data, err := instance.Marshal(inst)
		if err != nil {
			t.Fatal(err)
		}
		id, _, err := st.Put(data)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	f.signed = record("fetch", true)
	f.plain = record("fetch", false)
	f.sleeper = record("sleep", true)
	os.Unsetenv("STORE_TEST_CHILD")
	return f
}

func runReq(t *testing.T, h http.Handler, path, token, body string) (*httptest.ResponseRecorder, *RunResult) {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var res RunResult
	json.Unmarshal(w.Body.Bytes(), &res)
	return w, &res
}

func TestRunOnDemand(t *testing.T) {
	f := newRunFixture(t)
	t.Setenv("STORE_TEST_SECRET", "hunter2")
	hex := strings.TrimPrefix(f.signed, "sha256:")

	w, res := runReq(t, f.srv, "/v1/instances/"+hex[:12]+"/replay", "sesame", "")
	if w.Code != 200 || !res.Matched || res.Kind != "replay" || res.Instance != f.signed || !strings.HasPrefix(res.Stdout, "200 fine") {
		t.Fatalf("replay: %d %s", w.Code, w.Body)
	}
	// Runs start from a minimal environment: the server's own variables,
	// tokens included, don't reach the program.
	if strings.Contains(res.Stdout, "hunter2") {
		t.Fatal("the server's environment reached the program")
	}

	w, res = runReq(t, f.srv, "/v1/instances/"+hex+"/fork", "sesame", `{"at": 1, "answer": {"status": 503, "body": "busy"}}`)
	if w.Code != 200 || res.ExitCode != 1 || !strings.Contains(res.Stderr, "the page said 503") || res.Summary == nil || res.Summary.Given != 1 || res.Summary.Sent != 0 {
		t.Fatalf("fork: %d %s", w.Code, w.Body)
	}
	data, _, err := f.srv.Store.Get(res.Fork)
	if err != nil {
		t.Fatal(err)
	}
	kept, _ := instance.Parse(data)
	if kept.Fork == nil || kept.Fork.Parent != f.signed || kept.Fork.At != 1 || len(kept.Signatures) != 0 {
		t.Fatalf("the kept fork: %+v", kept.Fork)
	}
	// With a key, the server signs the forks it keeps.
	f.srv.Runner.SignKey, f.srv.Runner.Signer = f.key, "store"
	_, res = runReq(t, f.srv, "/v1/instances/"+hex+"/fork", "sesame", `{"at": 2}`)
	data, _, _ = f.srv.Store.Get(res.Fork)
	kept, _ = instance.Parse(data)
	if res.ExitCode != 0 || len(kept.Signatures) != 1 || kept.Signatures[0].Signer != "store" || res.Summary.FromParent != 1 {
		t.Fatalf("signed fork: %+v %+v", res, kept.Signatures)
	}

	for name, c := range map[string]struct {
		path, token, body, ctype string
		want                     int
	}{
		"unsigned":       {"/v1/instances/" + f.plain + "/replay", "sesame", "", "application/json", 403},
		"no token":       {"/v1/instances/" + hex + "/replay", "", "", "application/json", 401},
		"form post":      {"/v1/instances/" + hex + "/replay", "sesame", "", "application/x-www-form-urlencoded", 415},
		"unknown member": {"/v1/instances/" + hex + "/replay", "sesame", `{"argv": ["sh"]}`, "application/json", 400},
		"two values":     {"/v1/instances/" + hex + "/replay", "sesame", `{} {}`, "application/json", 400},
		"bad answer":     {"/v1/instances/" + hex + "/fork", "sesame", `{"at": 1, "answer": {"status": 99}}`, "application/json", 400},
		"bad step":       {"/v1/instances/" + hex + "/fork", "sesame", `{"at": 7}`, "application/json", 422},
		"no such run":    {"/v1/instances/" + strings.Repeat("0", 64) + "/replay", "sesame", "", "application/json", 404},
		"other verb":     {"/v1/instances/" + hex + "/delete", "sesame", "", "application/json", 404},
		"huge body":      {"/v1/instances/" + hex + "/fork", "sesame", `{"at": 1, "answer": {"status": 200, "body": "` + strings.Repeat("x", 2<<20) + `"}}`, "application/json", 413},
	} {
		req := httptest.NewRequest("POST", c.path, strings.NewReader(c.body))
		req.Header.Set("Content-Type", c.ctype)
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		w := httptest.NewRecorder()
		f.srv.ServeHTTP(w, req)
		if w.Code != c.want {
			t.Errorf("%s: %d, want %d: %s", name, w.Code, c.want, w.Body)
		}
	}
	if w := do(t, f.srv, "GET", "/v1/instances/"+hex+"/replay", "sesame", nil); w.Code != 405 {
		t.Errorf("GET on a run: %d", w.Code)
	}

	// A store without a runner, or without trusted keys, runs nothing.
	off := &Server{Store: f.srv.Store, Token: "sesame"}
	if w, _ := runReq(t, off, "/v1/instances/"+hex+"/replay", "sesame", ""); w.Code != 404 {
		t.Errorf("no runner: %d", w.Code)
	}
	untrusting := &Server{Store: f.srv.Store, Token: "sesame", Runner: &Runner{}}
	if w, _ := runReq(t, untrusting, "/v1/instances/"+hex+"/replay", "sesame", ""); w.Code != 403 {
		t.Errorf("no trusted keys: %d", w.Code)
	}
}

func TestRunLimits(t *testing.T) {
	f := newRunFixture(t)
	hex := strings.TrimPrefix(f.sleeper, "sha256:")
	var wg sync.WaitGroup
	codes := make([]int, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			time.Sleep(time.Duration(i) * 500 * time.Millisecond)
			w, _ := runReq(t, f.srv, "/v1/instances/"+hex+"/replay", "sesame", "")
			codes[i] = w.Code
		}(i)
	}
	wg.Wait()
	if codes[0] != 200 || codes[1] != 503 {
		t.Fatalf("one run at a time: %v", codes)
	}
	f.srv.Runner.Timeout = 300 * time.Millisecond
	start := time.Now()
	w, res := runReq(t, f.srv, "/v1/instances/"+hex+"/replay", "sesame", "")
	if w.Code != 200 || !res.TimedOut || res.Matched || time.Since(start) > 2500*time.Millisecond {
		t.Fatalf("time limit: %d %s", w.Code, w.Body)
	}
}

func TestRunClient(t *testing.T) {
	f := newRunFixture(t)
	ts := httptest.NewServer(f.srv)
	defer ts.Close()
	c := &Client{URL: ts.URL, Token: "sesame"}
	res, err := c.Replay(f.signed, ReplayRequest{})
	if err != nil || !res.Matched {
		t.Fatalf("%v %+v", err, res)
	}
	res, err = c.Fork(f.signed, ForkRequest{At: 1, Answer: &codeinstances.Answer{Error: "connection refused"}})
	if err != nil || res.ExitCode != 1 || !strings.HasPrefix(res.Fork, "sha256:") {
		t.Fatalf("%v %+v", err, res)
	}
	if _, err := c.Replay(f.plain, ReplayRequest{}); err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("unsigned: %v", err)
	}
	// A store that answers about another run is caught.
	liar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(RunResult{Instance: f.plain, Kind: "replay", Matched: true})
	}))
	defer liar.Close()
	if _, err := (&Client{URL: liar.URL}).Replay(f.signed, ReplayRequest{}); err == nil {
		t.Fatal("took an answer about another run")
	}
}

func TestTail(t *testing.T) {
	big := bytes.Repeat([]byte("é"), MaxRunOutput) // two bytes each
	s, cut := tail(instance.NewBody(big))
	if !cut || len(s) > MaxRunOutput || !strings.HasPrefix(s, "é") {
		t.Fatalf("%v %d %q", cut, len(s), s[:4])
	}
	if s, cut := tail(instance.NewBody([]byte("short"))); cut || s != "short" {
		t.Fatal(s)
	}
	if s, _ := tail(instance.NewBody([]byte{0xff, 'a'})); s != "�a" {
		t.Fatalf("%q", s)
	}
}
