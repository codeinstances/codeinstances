// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package capture

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codeinstances/codeinstances/instance"
	"github.com/codeinstances/codeinstances/internal/ca"
)

// upstream is a pair of live servers, HTTPS and HTTP, reached under the
// names secure.test and plain.test, whose answers change every request.
type upstream struct {
	tls, plain *httptest.Server
	auth       *ca.Authority
	hits       atomic.Int64
}

func newUpstream(t *testing.T) *upstream {
	t.Helper()
	auth, err := ca.New("upstream")
	if err != nil {
		t.Fatal(err)
	}
	u := &upstream{auth: auth}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := u.hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Set-Cookie", fmt.Sprintf("s=%d", n))
		w.Header().Set("X-Hit", fmt.Sprint(n))
		switch r.URL.Path {
		case "/binary":
			w.Write([]byte{0xff, 0x00, byte(n)})
		case "/big":
			w.Write(bytes.Repeat([]byte("x"), 8<<20))
		case "/empty":
			w.WriteHeader(http.StatusNoContent)
		default:
			fmt.Fprintf(w, "%s %s %s hit=%d ua=%q body=%s", r.Method, r.Host, r.URL.RequestURI(), n, r.UserAgent(), body)
		}
	})
	u.plain = httptest.NewServer(h)
	u.tls = httptest.NewUnstartedServer(h)
	cert, err := auth.Issue("secure.test")
	if err != nil {
		t.Fatal(err)
	}
	u.tls.TLS = &tls.Config{Certificates: []tls.Certificate{*cert}}
	u.tls.StartTLS()
	t.Cleanup(func() { u.tls.Close(); u.plain.Close() })
	return u
}

func (u *upstream) record(t *testing.T, keepSecrets bool) *Proxy {
	t.Helper()
	auth, err := ca.New("rec")
	if err != nil {
		t.Fatal(err)
	}
	p, err := StartRecord(RecordConfig{
		Authority:   auth,
		Resolve:     map[string]string{"secure.test": u.tls.Listener.Addr().String(), "plain.test": u.plain.Listener.Addr().String()},
		UpstreamTLS: &tls.Config{RootCAs: u.auth.Pool()},
		KeepSecrets: keepSecrets,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p
}

// client talks through the proxy and trusts its authority, as a recorded
// program does.
func client(p *Proxy, auth *ca.Authority) *http.Client {
	purl, _ := url.Parse(p.URL())
	return &http.Client{Timeout: 20 * time.Second, Transport: &http.Transport{
		Proxy:           http.ProxyURL(purl),
		TLSClientConfig: &tls.Config{RootCAs: auth.Pool()},
	}}
}

func do(t *testing.T, c *http.Client, method, u, body string) (int, http.Header, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, _ := http.NewRequest(method, u, r)
	req.Header.Set("Authorization", "Bearer secret-token")
	req.Header["User-Agent"] = []string{""} // send none; the proxy mustn't add one
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, u, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, string(b)
}

func replay(t *testing.T, calls []instance.Call, strict bool) (*Proxy, *http.Client) {
	t.Helper()
	auth, err := ca.New("rep")
	if err != nil {
		t.Fatal(err)
	}
	p, err := StartReplay(ReplayConfig{Authority: auth, Calls: calls, Strict: strict})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { p.Close() })
	return p, client(p, auth)
}

func TestRecordThenReplayHTTPAndHTTPS(t *testing.T) {
	u := newUpstream(t)
	p := u.record(t, false)
	c := client(p, p.auth)
	type ex struct{ method, url, body string }
	exs := []ex{
		{"GET", "https://secure.test/a?q=1", ""},
		{"POST", "https://secure.test/order", `{"n":1}`},
		{"GET", "http://plain.test/b", ""},
		{"PUT", "http://plain.test:80/c", "put"},
		{"GET", "https://secure.test/binary", ""},
		{"DELETE", "https://secure.test/d", ""},
		{"GET", "https://secure.test/empty", ""},
	}
	var live []string
	for _, e := range exs {
		code, _, body := do(t, c, e.method, e.url, e.body)
		live = append(live, fmt.Sprint(code, body))
	}
	p.Close()
	calls := p.Calls()
	if len(calls) != len(exs) {
		t.Fatalf("%d calls", len(calls))
	}
	for i, call := range calls {
		if call.Seq != i+1 || call.Kind != instance.KindHTTP || call.Response == nil {
			t.Fatalf("call %d: %+v", i, call)
		}
	}
	if calls[0].Request.URL != "https://secure.test/a?q=1" || calls[3].Request.URL != "http://plain.test/c" {
		t.Fatalf("urls %q %q", calls[0].Request.URL, calls[3].Request.URL)
	}
	if !strings.Contains(live[0], `ua=""`) {
		t.Fatalf("the proxy added a User-Agent: %s", live[0])
	}
	if calls[4].Response.Body.Base64 == "" {
		t.Fatal("binary body not base64")
	}
	if calls[6].Response.Status != 204 || calls[6].Response.Body != nil {
		t.Fatalf("204: %+v", calls[6].Response)
	}

	// The servers go away; the replay must not need them.
	u.tls.Close()
	u.plain.Close()
	hits := u.hits.Load()
	rp, rc := replay(t, calls, false)
	for i, e := range exs {
		code, _, body := do(t, rc, e.method, e.url, e.body)
		if got := fmt.Sprint(code, body); got != live[i] {
			t.Fatalf("replay %d: %q, recorded %q", i, got, live[i])
		}
	}
	if d := rp.Divergences(); len(d) != 0 {
		t.Fatal(d)
	}
	if rp.Served() != len(exs) || u.hits.Load() != hits {
		t.Fatalf("served %d, upstream hits %d -> %d", rp.Served(), hits, u.hits.Load())
	}
}

func TestSecretsAreRedactedUnlessKept(t *testing.T) {
	for _, keep := range []bool{false, true} {
		u := newUpstream(t)
		p := u.record(t, keep)
		do(t, client(p, p.auth), "GET", "https://secure.test/x", "")
		p.Close()
		call := p.Calls()[0]
		auth := call.Request.Header["Authorization"]
		cookie := call.Response.Header["Set-Cookie"]
		if keep {
			if auth[0] != "Bearer secret-token" || cookie[0] != "s=1" {
				t.Fatalf("kept: %v %v", auth, cookie)
			}
		} else if auth[0] != Redacted || cookie[0] != Redacted {
			t.Fatalf("redacted: %v %v", auth, cookie)
		}
		if call.Response.Header["X-Hit"][0] != "1" {
			t.Fatal("ordinary header lost")
		}
	}
}

func TestUpstreamErrorIsRecordedAndReplayed(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	auth, _ := ca.New("rec")
	p, err := StartRecord(RecordConfig{Authority: auth, Resolve: map[string]string{"gone.test": dead}})
	if err != nil {
		t.Fatal(err)
	}
	code, _, body := do(t, client(p, auth), "GET", "http://gone.test/x", "")
	p.Close()
	calls := p.Calls()
	if code != http.StatusBadGateway || len(calls) != 1 || calls[0].Error == "" || calls[0].Response != nil {
		t.Fatalf("%d %q %+v", code, body, calls)
	}
	_, rc := replay(t, calls, false)
	code2, h, body2 := do(t, rc, "GET", "http://gone.test/x", "")
	if code2 != code || body2 != body || h.Get("X-Codeinstances") != "upstream-error" {
		t.Fatalf("replayed %d %q, recorded %d %q", code2, body2, code, body)
	}
}

func TestReplayNotesUnknownAndMissingRequests(t *testing.T) {
	u := newUpstream(t)
	p := u.record(t, false)
	c := client(p, p.auth)
	do(t, c, "GET", "https://secure.test/one", "")
	do(t, c, "POST", "https://secure.test/two", "body-a")
	p.Close()
	rp, rc := replay(t, p.Calls(), false)
	code, h, _ := do(t, rc, "POST", "https://secure.test/two", "body-b")
	if code != http.StatusBadGateway || h.Get("X-Codeinstances") != "unmatched" {
		t.Fatalf("unknown request got %d", code)
	}
	d := rp.Divergences()
	if len(d) != 3 {
		t.Fatalf("divergences %q", d)
	}
	if !strings.Contains(d[0], "different body") || !strings.Contains(d[1], "never made") || !strings.Contains(d[2], "never made") {
		t.Fatalf("divergences %q", d)
	}
}

func TestRepeatedRequestsReplayInOrder(t *testing.T) {
	u := newUpstream(t)
	p := u.record(t, false)
	c := client(p, p.auth)
	var want []string
	for i := 0; i < 5; i++ {
		_, _, b := do(t, c, "GET", "https://secure.test/same", "")
		want = append(want, b)
	}
	p.Close()
	rp, rc := replay(t, p.Calls(), false)
	for i := range want {
		if _, _, b := do(t, rc, "GET", "https://secure.test/same", ""); b != want[i] {
			t.Fatalf("repeat %d: %q, want %q", i, b, want[i])
		}
	}
	// A sixth is one more than recorded.
	if code, _, _ := do(t, rc, "GET", "https://secure.test/same", ""); code != http.StatusBadGateway {
		t.Fatal("sixth request answered")
	}
	if d := rp.Divergences(); len(d) != 1 {
		t.Fatal(d)
	}
}

func TestConcurrentRequests(t *testing.T) {
	const n = 64
	u := newUpstream(t)
	p := u.record(t, false)
	c := client(p, p.auth)
	got := make([]string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, got[i] = do(t, c, "GET", fmt.Sprintf("https://secure.test/item/%d", i), "")
		}(i)
	}
	wg.Wait()
	p.Close()
	calls := p.Calls()
	if len(calls) != n {
		t.Fatalf("%d calls", len(calls))
	}
	for i := 0; i < 3; i++ {
		rp, rc := replay(t, calls, false)
		var wg sync.WaitGroup
		bad := atomic.Int64{}
		for i := n - 1; i >= 0; i-- {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if _, _, b := do(t, rc, "GET", fmt.Sprintf("https://secure.test/item/%d", i), ""); b != got[i] {
					bad.Add(1)
				}
			}(i)
		}
		wg.Wait()
		if bad.Load() != 0 || len(rp.Divergences()) != 0 || rp.Served() != n {
			t.Fatalf("round %d: %d wrong, %v", i, bad.Load(), rp.Divergences())
		}
	}
}

func TestStrictOrder(t *testing.T) {
	u := newUpstream(t)
	p := u.record(t, false)
	c := client(p, p.auth)
	do(t, c, "GET", "https://secure.test/first", "")
	do(t, c, "GET", "https://secure.test/second", "")
	p.Close()

	rp, rc := replay(t, p.Calls(), true)
	do(t, rc, "GET", "https://secure.test/first", "")
	do(t, rc, "GET", "https://secure.test/second", "")
	if d := rp.Divergences(); len(d) != 0 {
		t.Fatal(d)
	}
	rp, rc = replay(t, p.Calls(), true)
	if code, _, _ := do(t, rc, "GET", "https://secure.test/second", ""); code != http.StatusBadGateway {
		t.Fatal("out of order answered")
	}
	if d := rp.Divergences(); len(d) != 3 || !strings.Contains(d[0], "request 1 was GET https://secure.test/second") {
		t.Fatalf("%q", d)
	}
}

func TestLargeBodies(t *testing.T) {
	u := newUpstream(t)
	p := u.record(t, false)
	c := client(p, p.auth)
	big := strings.Repeat("y", 4<<20)
	_, _, up := do(t, c, "POST", "https://secure.test/upload", big)
	_, _, down := do(t, c, "GET", "https://secure.test/big", "")
	if len(down) != 8<<20 || !strings.HasSuffix(up, big[:100]+strings.Repeat("y", len(big)-100)) {
		t.Fatalf("live: %d %d", len(down), len(up))
	}
	p.Close()
	_, rc := replay(t, p.Calls(), false)
	if _, _, b := do(t, rc, "POST", "https://secure.test/upload", big); b != up {
		t.Fatal("upload replay differs")
	}
	if _, _, b := do(t, rc, "GET", "https://secure.test/big", ""); b != down {
		t.Fatal("download replay differs")
	}
}

func TestTooLargeBodyFailsThatExchange(t *testing.T) {
	u := newUpstream(t)
	p := u.record(t, false)
	c := client(p, p.auth)
	huge := strings.Repeat("z", MaxBody+1)
	req, _ := http.NewRequest("POST", "http://plain.test/huge", strings.NewReader(huge))
	resp, err := c.Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Fatalf("status %d", resp.StatusCode)
		}
	}
	p.Close()
	if len(p.Calls()) != 0 || len(p.Divergences()) != 1 {
		t.Fatalf("calls %d, notes %v", len(p.Calls()), p.Divergences())
	}
}

func TestNonProxyRequestIsRefused(t *testing.T) {
	u := newUpstream(t)
	p := u.record(t, false)
	resp, err := http.Get(p.URL() + "/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestCertificatesAreIssuedForTheRequestedHost(t *testing.T) {
	u := newUpstream(t)
	p := u.record(t, false)
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(p.auth.CertPEM())
	purl, _ := url.Parse(p.URL())
	c := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(purl), TLSClientConfig: &tls.Config{RootCAs: pool}}}
	resp, err := c.Get("https://secure.test/x")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.TLS == nil || resp.TLS.PeerCertificates[0].DNSNames[0] != "secure.test" {
		t.Fatal("wrong certificate")
	}
	// A client that doesn't trust the run's authority is refused.
	c2 := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(purl)}}
	if _, err := c2.Get("https://secure.test/x"); err == nil {
		t.Fatal("untrusted authority accepted")
	}
}

func TestCloseIsSafeTwiceAndRefusesLateRequests(t *testing.T) {
	u := newUpstream(t)
	p := u.record(t, false)
	c := client(p, p.auth)
	do(t, c, "GET", "https://secure.test/x", "")
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	p.Close()
	if _, err := c.Get("https://secure.test/y"); err == nil {
		t.Fatal("request after close succeeded")
	}
	if len(p.Calls()) != 1 {
		t.Fatal(len(p.Calls()))
	}
}

func TestHelpers(t *testing.T) {
	cases := map[[2]string]string{
		{"https", "Example.COM:443"}:  "example.com",
		{"http", "example.com:80"}:    "example.com",
		{"https", "example.com:8443"}: "example.com:8443",
		{"https", "[::1]:443"}:        "[::1]",
	}
	for in, want := range cases {
		if got := canonicalHost(in[0], in[1]); got != want {
			t.Errorf("canonicalHost%v = %q, want %q", in, got, want)
		}
	}
	r := map[string]string{"a.test": "127.0.0.1:1", "b.test:443": "127.0.0.1:2", "c.test": "10.0.0.1"}
	for in, want := range map[string]string{"a.test:443": "127.0.0.1:1", "b.test:443": "127.0.0.1:2", "b.test:80": "b.test:80", "c.test:8080": "10.0.0.1:8080", "d.test:1": "d.test:1"} {
		if got := resolveAddr(r, in); got != want {
			t.Errorf("resolveAddr(%q) = %q, want %q", in, got, want)
		}
	}
	h := http.Header{"Connection": {"close, X-Private"}, "X-Private": {"1"}, "Keep-Alive": {"x"}, "X-Ok": {"1"}}
	out := passable(h)
	if out.Get("X-Private") != "" || out.Get("Keep-Alive") != "" || out.Get("Connection") != "" || out.Get("X-Ok") != "1" {
		t.Fatal(out)
	}
	if key("GET", "u", nil) == key("GET", "u", []byte("x")) || key("GET", "u", nil) != key("GET", "u", []byte{}) {
		t.Fatal("key")
	}
}
