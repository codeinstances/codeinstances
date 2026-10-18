// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

// Command codeinstances-demo checks CodeInstances end to end and measures it.
//
// It starts two local servers (one HTTPS, one plain HTTP) whose answers
// change on every request, records a program that talks to both and uses the
// hook package for its clock and random numbers, shuts the servers down, and
// replays the instance with the codeinstances command. Then it changes the
// program in two ways the replay must catch, and edits the instance file in
// a way verify must catch. Progress goes to stderr, a JSON report to stdout,
// and any failed check makes it exit 1.
//
//	codeinstances-demo [-cli path/to/codeinstances] [-replays 5]
package main

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/codeinstances/codeinstances"
	"github.com/codeinstances/codeinstances/hook"
	"github.com/codeinstances/codeinstances/instance"
	"github.com/codeinstances/codeinstances/internal/buildinfo"
	"github.com/codeinstances/codeinstances/internal/ca"
	"github.com/codeinstances/codeinstances/store"
)

const (
	secureHost = "api.demo.test"
	plainHost  = "plain.demo.test"
	parallel   = 20
)

type check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

type report struct {
	Tool        buildinfo.Record `json:"tool"`
	CPUs        int              `json:"cpus"`
	Checks      []check          `json:"checks"`
	Passed      bool             `json:"passed"`
	Calls       int              `json:"calls"`
	Values      int              `json:"values"`
	FileBytes   int64            `json:"instance_file_bytes"`
	RecordMS    int64            `json:"record_ms"`
	ReplayMS    []int64          `json:"replay_ms"`
	DiffMS      int64            `json:"diff_ms"`
	ShrinkMS    int64            `json:"shrink_ms"`
	ShrinkRuns  int              `json:"shrink_runs"`
	Changed     int              `json:"shrink_answers_changed"`
	Needed      int              `json:"shrink_answers_needed"`
	TestMS      int64            `json:"test_ms"`
	GroupMS     int64            `json:"group_ms"`
	Groups      int              `json:"groups"`
	PushMS      int64            `json:"push_ms"`
	ForkMS      int64            `json:"fork_ms"`
	PullMS      int64            `json:"pull_ms"`
	SignMS      int64            `json:"sign_ms"`
	VerifyMS    int64            `json:"verify_ms"`
	RunMS       int64            `json:"run_ms"`
	RunForkMS   int64            `json:"run_fork_ms"`
	ServerCalls int              `json:"live_server_requests"`
}

func main() {
	child := flag.Bool("child", false, "run as the recorded program (used by the demo itself)")
	extra := flag.Bool("extra", false, "child: make one request more than recorded")
	unhooked := flag.Bool("unhooked", false, "child: print the real clock instead of the hooked one")
	tolerant := flag.Bool("tolerant", false, "child: carry on when an item can't be read (the fixed build)")
	cli := flag.String("cli", "", "the codeinstances command (default: next to this one, then PATH)")
	replays := flag.Int("replays", 5, "how many times to replay the instance")
	flag.Parse()
	if *child {
		os.Exit(runChild(*extra, *unhooked, *tolerant))
	}
	rep, err := demo(*cli, *replays)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if rep != nil {
		enc.Encode(rep)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "codeinstances-demo:", err)
		os.Exit(1)
	}
	if !rep.Passed {
		fmt.Fprintln(os.Stderr, "codeinstances-demo: a check failed")
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "codeinstances-demo: every check passed")
}

// runChild is the program being recorded and replayed.
func runChild(extra, unhooked, tolerant bool) int {
	client := hook.Client()
	client.Timeout = 30 * time.Second
	start := hook.Now()
	if unhooked {
		fmt.Println("started", time.Now().Format(time.RFC3339Nano))
	} else {
		fmt.Println("started", start.Format(time.RFC3339Nano))
	}

	quote, err := get(client, "https://"+secureHost+"/quote")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("quote", quote)

	orderID := hook.Uint64()
	body := fmt.Sprintf(`{"order":%d,"qty":%d}`, orderID, 1+hook.Intn(9))
	resp, err := client.Post("https://"+secureHost+"/order", "application/json", strings.NewReader(body))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	fmt.Println("order", resp.StatusCode, string(b))
	if resp.StatusCode != http.StatusCreated && !tolerant {
		fmt.Fprintf(os.Stderr, "the order was refused: %d\n", resp.StatusCode)
		return 1
	}

	status, err := get(client, "http://"+plainHost+"/status")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	fmt.Println("status", status)

	// Requests from many goroutines at once: they reach the proxy in a
	// different order every run.
	items := make([]string, parallel)
	var wg sync.WaitGroup
	for i := range items {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			items[i], _ = get(client, fmt.Sprintf("https://%s/item/%d", secureHost, i))
		}(i)
	}
	wg.Wait()
	failed := 0
	for i, it := range items {
		fmt.Printf("item %d %s\n", i, it)
		if !strings.HasPrefix(it, "200 ") {
			failed++
		}
	}
	if failed > 0 && !tolerant {
		fmt.Fprintf(os.Stderr, "%d items couldn't be read\n", failed)
		return 1
	}

	if extra {
		get(client, "https://"+secureHost+"/quote?again=1")
	}
	token := make([]byte, 16)
	hook.Read(token)
	fmt.Printf("token %x\n", token)
	fmt.Println("took", hook.Since(start) >= 0)
	return 0
}

func get(c *http.Client, url string) (string, error) {
	resp, err := c.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return fmt.Sprintf("%d %s", resp.StatusCode, bytes.TrimSpace(b)), err
}

// servers answer differently every time, so a replay that matches can only
// have come from the recording.
type servers struct {
	mu     sync.Mutex
	count  int
	broken bool // item 7 fails, as a bug in the service would make it
	refuse bool // orders are refused, a second bug
	ln     []net.Listener
	srv    []*http.Server
}

func (s *servers) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.count++
	n := s.count
	broken, refuse := s.broken, s.refuse
	s.mu.Unlock()
	stamp := time.Now().UnixNano()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Set-Cookie", fmt.Sprintf("session=%d", stamp))
	switch {
	case refuse && r.URL.Path == "/order":
		w.WriteHeader(http.StatusServiceUnavailable)
		fmt.Fprintf(w, `{"error":"try later","at":%d}`, stamp)
	case r.URL.Path == "/order" && r.Method == http.MethodPost:
		b, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"accepted":%s,"at":%d,"n":%d}`, b, stamp, n)
	case r.URL.Path == "/quote":
		fmt.Fprintf(w, `{"price":%d,"at":%d}`, 100+stamp%900, stamp)
	case r.URL.Path == "/status":
		fmt.Fprintf(w, `{"ok":true,"served":%d}`, n)
	case broken && r.URL.Path == "/item/7":
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprintf(w, `{"error":"out of service","at":%d}`, stamp)
	case strings.HasPrefix(r.URL.Path, "/item/"):
		fmt.Fprintf(w, `{"item":%q,"stock":%d}`, strings.TrimPrefix(r.URL.Path, "/item/"), stamp%97)
	default:
		http.NotFound(w, r)
	}
}

func (s *servers) start(auth *ca.Authority) (secureAddr, plainAddr string, err error) {
	cert, err := auth.Issue(secureHost)
	if err != nil {
		return "", "", err
	}
	tl, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{*cert}})
	if err != nil {
		return "", "", err
	}
	pl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tl.Close()
		return "", "", err
	}
	for _, l := range []net.Listener{tl, pl} {
		// Connections the recorder's proxy drops when a run ends would
		// otherwise be logged as handshake errors.
		srv := &http.Server{Handler: http.HandlerFunc(s.handler), ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
		s.ln = append(s.ln, l)
		s.srv = append(s.srv, srv)
		go srv.Serve(l)
	}
	return tl.Addr().String(), pl.Addr().String(), nil
}

func (s *servers) stop() {
	for _, srv := range s.srv {
		srv.Close()
	}
}

func demo(cli string, replays int) (*report, error) {
	rep := &report{Tool: buildinfo.Info(), CPUs: runtime.NumCPU()}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if cli == "" {
		cli, err = findCLI(self)
		if err != nil {
			return nil, err
		}
	}
	work, err := os.MkdirTemp("", "codeinstances-demo-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)

	add := func(name string, ok bool, detail string) {
		rep.Checks = append(rep.Checks, check{Name: name, Passed: ok, Detail: detail})
		mark := "ok  "
		if !ok {
			mark = "FAIL"
		}
		fmt.Fprintf(os.Stderr, "%s %s %s\n", mark, name, detail)
	}

	// The servers' own certificate authority, which the recording trusts
	// like any upstream CA.
	auth, err := ca.New("Demo servers")
	if err != nil {
		return nil, err
	}
	caFile := filepath.Join(work, "servers-ca.pem")
	if err := os.WriteFile(caFile, auth.CertPEM(), 0o600); err != nil {
		return nil, err
	}
	srv := &servers{}
	secureAddr, plainAddr, err := srv.start(auth)
	if err != nil {
		return nil, err
	}
	defer srv.stop()

	file := filepath.Join(work, "demo.instance")
	t := time.Now()
	out, code, err := runCLI(cli, "record", "-q", "-o", file,
		"-resolve", secureHost+"="+secureAddr, "-resolve", plainHost+"="+plainAddr,
		"-upstream-ca", caFile, "--", self, "-child")
	rep.RecordMS = time.Since(t).Milliseconds()
	if err != nil {
		return rep, err
	}
	add("record finishes and keeps the exit code", code == 0, fmt.Sprintf("exit %d %s", code, lastLine(out)))
	inst, err := instance.Load(file)
	if err != nil {
		add("instance file loads", false, err.Error())
		return finish(rep), nil
	}
	if fi, err := os.Stat(file); err == nil {
		rep.FileBytes = fi.Size()
	}
	rep.Calls, rep.Values = len(inst.Calls), len(inst.Values)
	srv.mu.Lock()
	rep.ServerCalls = srv.count
	srv.mu.Unlock()
	add("every request reached the live servers and was kept", rep.Calls == 3+parallel && rep.ServerCalls == rep.Calls, fmt.Sprintf("%d calls kept, %d served live", rep.Calls, rep.ServerCalls))
	add("hooked clock and random values were kept", rep.Values == 5, fmt.Sprintf("%d values", rep.Values))
	add("secret headers are redacted", allRedacted(inst), "Set-Cookie")
	stdout := string(inst.Result.Stdout.MustBytes())
	add("the program saw HTTPS and HTTP answers", strings.Contains(stdout, "order 201") && strings.Contains(stdout, `status 200 {"ok":true`), "")

	// A second run, of a program that makes one request more, to compare.
	second := filepath.Join(work, "second.instance")
	_, code, err = runCLI(cli, "record", "-q", "-o", second,
		"-resolve", secureHost+"="+secureAddr, "-resolve", plainHost+"="+plainAddr,
		"-upstream-ca", caFile, "--", self, "-child", "-extra")
	if err != nil {
		return rep, err
	}
	if code != 0 {
		add("a second run is recorded", false, fmt.Sprintf("exit %d", code))
	}

	// A failing run: the service breaks item 7 and the program exits 1.
	srv.mu.Lock()
	srv.broken = true
	srv.mu.Unlock()
	failing := filepath.Join(work, "failing.instance")
	_, code, err = runCLI(cli, "record", "-q", "-o", failing,
		"-resolve", secureHost+"="+secureAddr, "-resolve", plainHost+"="+plainAddr,
		"-upstream-ca", caFile, "--", self, "-child")
	if err != nil {
		return rep, err
	}
	add("a failing run is recorded", code == 1, fmt.Sprintf("exit %d", code))

	// Two more failing runs: the same bug again, then the item fixed and a
	// second bug, orders refused.
	again := filepath.Join(work, "failing-again.instance")
	refused := filepath.Join(work, "refused.instance")
	codes := []int{}
	for _, f := range []string{again, refused} {
		if f == refused {
			srv.mu.Lock()
			srv.broken, srv.refuse = false, true
			srv.mu.Unlock()
		}
		_, code, err = runCLI(cli, "record", "-q", "-o", f,
			"-resolve", secureHost+"="+secureAddr, "-resolve", plainHost+"="+plainAddr,
			"-upstream-ca", caFile, "--", self, "-child")
		if err != nil {
			return rep, err
		}
		codes = append(codes, code)
	}
	add("two more failing runs are recorded", codes[0] == 1 && codes[1] == 1, fmt.Sprintf("exits %v", codes))

	// From here on nothing is listening.
	srv.stop()
	if c, err := net.DialTimeout("tcp", secureAddr, time.Second); err == nil {
		c.Close()
		add("live servers are down before replay", false, "")
	} else {
		add("live servers are down before replay", true, "")
	}

	allMatched := true
	for i := 0; i < replays; i++ {
		t := time.Now()
		out, code, err := runCLI(cli, "replay", "-json", file)
		rep.ReplayMS = append(rep.ReplayMS, time.Since(t).Milliseconds())
		if err != nil {
			return rep, err
		}
		var r struct {
			Matched     bool     `json:"matched"`
			Divergences []string `json:"divergences"`
		}
		json.Unmarshal([]byte(out), &r)
		if code != 0 || !r.Matched {
			allMatched = false
			add(fmt.Sprintf("replay %d matches", i+1), false, strings.Join(r.Divergences, "; "))
		}
	}
	add(fmt.Sprintf("%d replays offline all match the recording", replays), allMatched, "")

	out, code, err = runCLI(cli, "replay", "-json", file, "--", self, "-child", "-extra")
	if err != nil {
		return rep, err
	}
	add("an extra request is caught", code == 1 && strings.Contains(out, "isn't in the recording"), firstDivergence(out))

	out, code, err = runCLI(cli, "replay", "-json", file, "--", self, "-child", "-unhooked")
	if err != nil {
		return rep, err
	}
	add("an unhooked clock is caught", code == 1 && strings.Contains(out, "stdout line 1"), firstDivergence(out))

	_, code, err = runCLI(cli, "verify", file)
	if err != nil {
		return rep, err
	}
	add("verify accepts the untouched file", code == 0, "")
	data, _ := os.ReadFile(file)
	edited := filepath.Join(work, "edited.instance")
	changed := bytes.Replace(data, []byte(`\"qty\":`), []byte(`\"qty\":1`), 1)
	os.WriteFile(edited, changed, 0o600)
	_, code, err = runCLI(cli, "verify", edited)
	if err != nil {
		return rep, err
	}
	add("verify catches an edited file", !bytes.Equal(data, changed) && code == 1, "")

	out, code, err = runCLI(cli, "diff", "-json", file, file)
	if err != nil {
		return rep, err
	}
	add("diff finds a run the same as itself", code == 0 && strings.Contains(out, `"same": true`), "")
	t = time.Now()
	out, code, err = runCLI(cli, "diff", "-json", file, second)
	rep.DiffMS = time.Since(t).Milliseconds()
	if err != nil {
		return rep, err
	}
	var cmp struct {
		Calls struct {
			Changed int `json:"changed"`
			OnlyA   int `json:"only_a"`
			OnlyB   int `json:"only_b"`
		} `json:"calls"`
		Differences []string `json:"differences"`
		Speed       struct {
			Calls []json.RawMessage `json:"calls"`
		} `json:"speed"`
	}
	json.Unmarshal([]byte(out), &cmp)
	extraFound := false
	for _, d := range cmp.Differences {
		extraFound = extraFound || strings.Contains(d, "/quote?again=1 (b call") && strings.HasSuffix(d, "is only in b")
	}
	add("diff finds the extra request in a second run", code == 1 && extraFound && cmp.Calls.OnlyA == 0 && cmp.Calls.OnlyB == 1,
		fmt.Sprintf("%d differences, %d calls changed, %d only in b", len(cmp.Differences), cmp.Calls.Changed, cmp.Calls.OnlyB))
	add("diff times every paired call", len(cmp.Speed.Calls) == 3+parallel, fmt.Sprintf("%d calls timed in both runs", len(cmp.Speed.Calls)))

	// Shrink the failing run against the first one: of all the answers that
	// differ, only item 7's should be needed.
	shrunk := filepath.Join(work, "tests", "item7.instance")
	os.MkdirAll(filepath.Dir(shrunk), 0o700)
	t = time.Now()
	out, code, err = runCLI(cli, "shrink", "-q", "-json", "-pass", file, "-o", shrunk, failing)
	rep.ShrinkMS = time.Since(t).Milliseconds()
	if err != nil {
		return rep, err
	}
	var sh struct {
		Changed int      `json:"changed"`
		Needed  []string `json:"needed"`
		Runs    int      `json:"runs"`
		Minimal bool     `json:"minimal"`
	}
	json.Unmarshal([]byte(out), &sh)
	rep.ShrinkRuns, rep.Changed, rep.Needed = sh.Runs, sh.Changed, len(sh.Needed)
	add("shrink narrows the failure to item 7's answer", code == 0 && sh.Minimal && len(sh.Needed) == 1 && strings.Contains(sh.Needed[0], "/item/7 ") && sh.Changed > 10,
		fmt.Sprintf("%d of %d differing answers needed, %d runs", len(sh.Needed), sh.Changed, sh.Runs))

	// The first run and the shrunk failure as regression tests: the program
	// as it is fails the bug's test, the fixed build passes both.
	os.Rename(file, filepath.Join(work, "tests", "orders.instance"))
	out, code, err = runCLI(cli, "test", filepath.Join(work, "tests"))
	if err != nil {
		return rep, err
	}
	add("test fails the build that still has the bug", code == 1 && strings.Contains(out, "1 of 2 tests passed"), lastLine(out))
	t = time.Now()
	out, code, err = runCLI(cli, "test", filepath.Join(work, "tests"), "--", self, "-child", "-tolerant")
	rep.TestMS = time.Since(t).Milliseconds()
	if err != nil {
		return rep, err
	}
	add("test passes the fixed build", code == 0 && strings.Contains(out, "2 of 2 tests passed"), lastLine(out))

	// Group the three failing runs and a passing one: the two runs with the
	// item 7 bug are one group, the refused order another.
	t = time.Now()
	out, code, err = runCLI(cli, "group", "-json", failing, again, refused, second)
	rep.GroupMS = time.Since(t).Milliseconds()
	if err != nil {
		return rep, err
	}
	var grp struct {
		Groups []struct {
			Runs []string `json:"runs"`
		} `json:"groups"`
		Failing int `json:"failing"`
		Passed  int `json:"passed"`
	}
	json.Unmarshal([]byte(out), &grp)
	rep.Groups = len(grp.Groups)
	add("group puts repeats of one bug together", code == 0 && grp.Failing == 3 && grp.Passed == 1 && len(grp.Groups) == 2 &&
		len(grp.Groups[0].Runs) == 2 && grp.Groups[1].Runs[0] == refused,
		fmt.Sprintf("%d failing runs in %d groups", grp.Failing, len(grp.Groups)))

	// Share and store: a store on a loopback port with a token, the three
	// failing runs pushed to it, one pulled back by a short id prefix, and
	// the store's own grouping.
	st, err := store.Open(filepath.Join(work, "store"))
	if err != nil {
		return rep, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return rep, err
	}
	storeSrv := &http.Server{Handler: &store.Server{Store: st, Token: "demo-token"}, ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	go storeSrv.Serve(ln)
	defer storeSrv.Close()
	storeURL := "http://" + ln.Addr().String()
	t = time.Now()
	out, code, err = runCLI(cli, "push", "-json", "-to", storeURL, "-token", "demo-token", failing, again, refused)
	rep.PushMS = time.Since(t).Milliseconds()
	if err != nil {
		return rep, err
	}
	var pushed []struct {
		ID      string `json:"id"`
		Created bool   `json:"created"`
	}
	json.Unmarshal([]byte(out), &pushed)
	add("push uploads the failing runs to a store", code == 0 && len(pushed) == 3 && pushed[0].Created, fmt.Sprintf("%d pushed", len(pushed)))
	if len(pushed) == 3 {
		pulled := filepath.Join(work, "pulled.instance")
		t = time.Now()
		_, code, err = runCLI(cli, "pull", "-from", storeURL, "-token", "demo-token", "-o", pulled, strings.TrimPrefix(pushed[2].ID, "sha256:")[:12])
		rep.PullMS = time.Since(t).Milliseconds()
		if err != nil {
			return rep, err
		}
		a, _ := os.ReadFile(refused)
		b, _ := os.ReadFile(pulled)
		add("pull fetches a run back by a 12-digit id prefix", code == 0 && bytes.Equal(a, b), "")
	}
	g := st.Groups()
	add("the store groups what it holds", g.Failing == 3 && len(g.Groups) == 2, "")
	out, code, err = runCLI(cli, "report", "-url", storeURL, refused)
	if err != nil {
		return rep, err
	}
	add("report writes a bug report that says how to pull and replay the run",
		code == 0 && strings.HasPrefix(out, "## exit 1: `the order was refused") && strings.Contains(out, "codeinstances pull -from "+storeURL), "")

	// Fork the first run with the servers gone. Offline at its order call,
	// with a 503 given as the answer: a what-if. And one past its last call:
	// every answer from the parent, so it comes out the same.
	orders := filepath.Join(work, "tests", "orders.instance")
	parent, err := instance.Load(orders)
	if err != nil {
		return rep, err
	}
	step := 0
	for _, c := range parent.Calls {
		if strings.HasSuffix(c.Request.URL, "/order") {
			step = c.Seq
		}
	}
	answer := filepath.Join(work, "refused.json")
	os.WriteFile(answer, []byte(`{"status": 503, "body": "{\"error\":\"try later\"}"}`), 0o600)
	whatIf := filepath.Join(work, "what-if.instance")
	t = time.Now()
	out, code, err = runCLI(cli, "fork", "-q", "-offline", "-at", fmt.Sprint(step), "-answer", answer, "-o", whatIf, orders)
	rep.ForkMS = time.Since(t).Milliseconds()
	if err != nil {
		return rep, err
	}
	f, ferr := instance.Load(whatIf)
	add("fork with a given answer runs the what-if offline", code == 1 && ferr == nil && f.Fork != nil && f.Fork.At == step &&
		strings.Contains(string(f.Result.Stderr.MustBytes()), "the order was refused: 503"), lastLine(out))
	same := filepath.Join(work, "same.instance")
	_, code, err = runCLI(cli, "fork", "-q", "-offline", "-at", fmt.Sprint(len(parent.Calls)+1), "-o", same, orders)
	if err != nil {
		return rep, err
	}
	_, diffCode, err := runCLI(cli, "diff", orders, same)
	if err != nil {
		return rep, err
	}
	add("fork past the last call answers every call from the parent and comes out the same", code == 0 && diffCode == 0, "")

	// Proof: sign the refused run with a fresh key, check it against that
	// key, catch an edit made with a fresh id, and a store that takes only
	// signed runs.
	keyBase := filepath.Join(work, "ci")
	if _, code, err = runCLI(cli, "keygen", "-o", keyBase, "-label", "demo-ci"); err != nil || code != 0 {
		return rep, fmt.Errorf("keygen: exit %d %v", code, err)
	}
	runCLI(cli, "keygen", "-o", filepath.Join(work, "someone"))
	signed := filepath.Join(work, "signed.instance")
	t = time.Now()
	_, code, err = runCLI(cli, "sign", "-key", keyBase+".key", "-signer", "demo-ci", "-o", signed, refused)
	rep.SignMS = time.Since(t).Milliseconds()
	if err != nil {
		return rep, err
	}
	t = time.Now()
	var vcode int
	out, vcode, err = runCLI(cli, "verify", "-trust", keyBase+".pub", signed)
	rep.VerifyMS = time.Since(t).Milliseconds()
	if err != nil {
		return rep, err
	}
	add("sign vouches for a run and verify -trust accepts the signer's key", code == 0 && vcode == 0 && strings.Contains(out, "trusted as demo-ci"), lastLine(out))
	_, c1, _ := runCLI(cli, "verify", "-trust", keyBase+".pub", refused)
	_, c2, _ := runCLI(cli, "verify", "-trust", filepath.Join(work, "someone.pub"), signed)
	add("verify -trust refuses an unsigned run and one signed by a key it doesn't trust", c1 == 1 && c2 == 1, "")
	sdata, _ := os.ReadFile(signed)
	sinst, _ := instance.Parse(sdata)
	fdata := bytes.Replace(sdata, []byte("the order was refused"), []byte("the order went through"), 1)
	newID, _ := instance.ComputeID(fdata)
	fdata = bytes.Replace(fdata, []byte(sinst.ID), []byte(newID), 1)
	forged := filepath.Join(work, "forged.instance")
	os.WriteFile(forged, fdata, 0o644)
	out, code, err = runCLI(cli, "verify", forged)
	if err != nil {
		return rep, err
	}
	add("an edited run with a fresh id still fails, on its signature", code == 1 && sinst != nil && !bytes.Equal(fdata, sdata) && strings.Contains(out, "signature"), lastLine(out))
	trusted, err := codeinstances.ReadTrust(keyBase + ".pub")
	if err != nil {
		return rep, err
	}
	st2, err := store.Open(filepath.Join(work, "signed-store"))
	if err != nil {
		return rep, err
	}
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return rep, err
	}
	// The store signs the forks it keeps with a key of its own.
	storeKey, err := codeinstances.GenerateKey()
	if err != nil {
		return rep, err
	}
	signedSrv := &http.Server{Handler: &store.Server{Store: st2, Token: "demo-token", Trusted: trusted,
		Runner: &store.Runner{Trusted: trusted, Answers: true, SignKey: storeKey, Signer: "demo-store"}}, ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	go signedSrv.Serve(ln2)
	defer signedSrv.Close()
	url2 := "http://" + ln2.Addr().String()
	_, c1, _ = runCLI(cli, "push", "-to", url2, "-token", "demo-token", refused)
	_, c2, _ = runCLI(cli, "push", "-to", url2, "-token", "demo-token", signed)
	add("a store started with trusted keys refuses the unsigned run and takes the signed one", c1 == 2 && c2 == 0 && len(st2.List()) == 1, "")

	// Runs on demand: the store replays the signed run it keeps, offline,
	// and tries a what-if on it: had the order gone through, the program
	// would have got as far as the status call, which the run never made.
	t = time.Now()
	out, code, err = runCLI(cli, "run", "-on", url2, "-token", "demo-token", sinst.ID)
	rep.RunMS = time.Since(t).Milliseconds()
	if err != nil {
		return rep, err
	}
	add("a store started with -run replays the signed run on demand, and it matches", code == 0 && strings.Contains(out, "on the store: matched (exit 1"), lastLine(out))
	accepted := filepath.Join(work, "accepted.json")
	os.WriteFile(accepted, []byte(`{"status": 201, "body": "{\"accepted\":true}"}`), 0o600)
	ordered := 0
	for _, c := range sinst.Calls {
		if strings.HasSuffix(c.Request.URL, "/order") {
			ordered = c.Seq
		}
	}
	t = time.Now()
	out, code, err = runCLI(cli, "run", "-on", url2, "-token", "demo-token", "-json", "-keep", "-at", fmt.Sprint(ordered), "-answer", accepted, sinst.ID)
	rep.RunForkMS = time.Since(t).Milliseconds()
	if err != nil {
		return rep, err
	}
	var ran store.RunResult
	json.Unmarshal([]byte(out[:strings.LastIndex(out, "}")+1]), &ran)
	kept, _, kerr := st2.Get(ran.Fork)
	var keptInst *instance.Instance
	if kerr == nil {
		keptInst, kerr = instance.Parse(kept)
	}
	add("run -at tries a what-if on the store: with the order accepted, the program gets as far as the status call",
		code == 1 && strings.Contains(ran.Stdout, "order 201") && ran.Summary != nil && ran.Summary.Given == 1 && ran.Summary.Offline >= 1 &&
			kerr == nil && keptInst.Fork != nil && len(keptInst.Signatures) == 1 && keptInst.Signatures[0].Signer == "demo-store",
		fmt.Sprintf("exit %d", ran.ExitCode))
	unsigned, _ := os.ReadFile(refused)
	st2.Put(unsigned)
	_, code, _ = runCLI(cli, "run", "-on", url2, "-token", "demo-token", idOfFile(refused))
	add("the store won't run a run no trusted key signed", code == 2, "")
	return finish(rep), nil
}

func idOfFile(path string) string {
	inst, err := instance.Load(path)
	if err != nil {
		return ""
	}
	return inst.ID
}

func finish(rep *report) *report {
	rep.Passed = len(rep.Checks) > 0
	for _, c := range rep.Checks {
		rep.Passed = rep.Passed && c.Passed
	}
	return rep
}

func allRedacted(inst *instance.Instance) bool {
	seen := false
	for _, c := range inst.Calls {
		if c.Response == nil {
			continue
		}
		for k, vs := range c.Response.Header {
			if strings.EqualFold(k, "Set-Cookie") {
				seen = true
				for _, v := range vs {
					if v != "[redacted]" {
						return false
					}
				}
			}
		}
	}
	return seen
}

func firstDivergence(out string) string {
	var r struct {
		Divergences []string `json:"divergences"`
	}
	json.Unmarshal([]byte(out), &r)
	if len(r.Divergences) == 0 {
		return ""
	}
	return r.Divergences[0]
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return s[i+1:]
	}
	return s
}

// runCLI runs the codeinstances command and returns its combined output and
// exit code; err is set only when it couldn't run.
func runCLI(cli string, args ...string) (string, int, error) {
	cmd := exec.Command(cli, args...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return out.String(), exit.ExitCode(), nil
	}
	if err != nil {
		return out.String(), -1, err
	}
	return out.String(), 0, nil
}

func findCLI(self string) (string, error) {
	name := "codeinstances"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	next := filepath.Join(filepath.Dir(self), name)
	if _, err := os.Stat(next); err == nil {
		return next, nil
	}
	if p, err := exec.LookPath("codeinstances"); err == nil {
		return p, nil
	}
	return "", errors.New("can't find the codeinstances command; pass -cli")
}
