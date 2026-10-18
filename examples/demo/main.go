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
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/codeinstances/codeinstances/hook"
	"github.com/codeinstances/codeinstances/instance"
	"github.com/codeinstances/codeinstances/internal/buildinfo"
	"github.com/codeinstances/codeinstances/internal/ca"
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
	ServerCalls int              `json:"live_server_requests"`
}

func main() {
	child := flag.Bool("child", false, "run as the recorded program (used by the demo itself)")
	extra := flag.Bool("extra", false, "child: make one request more than recorded")
	unhooked := flag.Bool("unhooked", false, "child: print the real clock instead of the hooked one")
	cli := flag.String("cli", "", "the codeinstances command (default: next to this one, then PATH)")
	replays := flag.Int("replays", 5, "how many times to replay the instance")
	flag.Parse()
	if *child {
		os.Exit(runChild(*extra, *unhooked))
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
func runChild(extra, unhooked bool) int {
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
	for i, it := range items {
		fmt.Printf("item %d %s\n", i, it)
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
	mu    sync.Mutex
	count int
	ln    []net.Listener
	srv   []*http.Server
}

func (s *servers) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.count++
	n := s.count
	s.mu.Unlock()
	stamp := time.Now().UnixNano()
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Set-Cookie", fmt.Sprintf("session=%d", stamp))
	switch {
	case r.URL.Path == "/order" && r.Method == http.MethodPost:
		b, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"accepted":%s,"at":%d,"n":%d}`, b, stamp, n)
	case r.URL.Path == "/quote":
		fmt.Fprintf(w, `{"price":%d,"at":%d}`, 100+stamp%900, stamp)
	case r.URL.Path == "/status":
		fmt.Fprintf(w, `{"ok":true,"served":%d}`, n)
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
		srv := &http.Server{Handler: http.HandlerFunc(s.handler), ReadHeaderTimeout: 10 * time.Second}
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
	return finish(rep), nil
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
