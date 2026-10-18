// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package capture is the HTTP proxy a recorded program talks through.
//
// When recording, the proxy passes each request on to the real server and
// keeps both sides of the exchange. When replaying, it never opens a
// connection: it answers each request with the response recorded for it and
// notes every request it has no answer for. HTTPS is handled by standing in
// for the server with a certificate from a per-run authority (package ca),
// which the program is told to trust.
package capture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codeinstances/codeinstances/instance"
	"github.com/codeinstances/codeinstances/internal/ca"
)

// MaxBody is the largest request or response body the proxy keeps. A bigger
// one fails that exchange.
const MaxBody = 64 << 20

// Redacted replaces the values of the headers in SecretHeaders.
const Redacted = "[redacted]"

// SecretHeaders are kept out of instance files unless KeepSecrets is set.
var SecretHeaders = []string{
	"Authorization", "Proxy-Authorization", "Cookie", "Set-Cookie",
	"X-Api-Key", "Api-Key", "X-Auth-Token", "X-Amz-Security-Token",
}

// hopHeaders belong to one connection and are never passed on.
var hopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// RecordConfig sets up a recording proxy.
type RecordConfig struct {
	Authority *ca.Authority
	// Resolve sends a host ("api.example.com" or "api.example.com:443") to
	// another address ("127.0.0.1:8443"), like curl --resolve. The original
	// host name is still used for TLS and the Host header.
	Resolve map[string]string
	// UpstreamTLS is used to reach real HTTPS servers. Nil means the system's
	// trusted roots.
	UpstreamTLS *tls.Config
	// KeepSecrets keeps SecretHeaders in the instance as they were.
	KeepSecrets bool
}

// ReplayConfig sets up a replaying proxy.
type ReplayConfig struct {
	Authority *ca.Authority
	Calls     []instance.Call
	// Strict requires requests to arrive in recorded order. Without it, each
	// request gets the next unused response recorded for the same method, URL
	// and body, whatever order requests from different threads arrive in.
	Strict bool
}

// Proxy is a running proxy. Close it when the program has exited.
type Proxy struct {
	auth   *ca.Authority
	replay bool
	start  time.Time

	// recording
	transport   *http.Transport
	keepSecrets bool

	// replaying
	recorded []instance.Call
	queues   map[string][]int
	used     []bool
	strict   bool
	next     int

	mu          sync.Mutex
	inflight    int
	closing     bool
	drained     chan struct{}
	seq         int
	calls       []instance.Call
	divergences []string
	served      int
	requests    int

	ln      net.Listener
	srv     *http.Server
	tunnels *http.Server
	connsMu sync.Mutex
	conns   map[net.Conn]struct{}
	closed  bool
}

// StartRecord starts a recording proxy on a loopback port.
func StartRecord(cfg RecordConfig) (*Proxy, error) {
	if cfg.Authority == nil {
		return nil, errors.New("capture: no authority")
	}
	upstream := cfg.UpstreamTLS
	if upstream == nil {
		upstream = &tls.Config{}
	}
	resolve := map[string]string{}
	for k, v := range cfg.Resolve {
		resolve[strings.ToLower(k)] = v
	}
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	p := &Proxy{
		auth:        cfg.Authority,
		keepSecrets: cfg.KeepSecrets,
		transport: &http.Transport{
			// codeinstances itself reaches the world the way its own
			// environment says, through a proxy if one is set, except for
			// hosts sent elsewhere with Resolve.
			Proxy: func(r *http.Request) (*url.URL, error) {
				if resolveAddr(resolve, hostPort(r.URL)) != hostPort(r.URL) {
					return nil, nil
				}
				return http.ProxyFromEnvironment(r)
			},
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.DialContext(ctx, network, resolveAddr(resolve, addr))
			},
			TLSClientConfig:       upstream.Clone(),
			ForceAttemptHTTP2:     true,
			DisableCompression:    true,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       30 * time.Second,
			TLSHandshakeTimeout:   15 * time.Second,
			ExpectContinueTimeout: time.Second,
		},
	}
	return p, p.listen()
}

// StartReplay starts a replaying proxy on a loopback port.
func StartReplay(cfg ReplayConfig) (*Proxy, error) {
	if cfg.Authority == nil {
		return nil, errors.New("capture: no authority")
	}
	p := &Proxy{
		auth:     cfg.Authority,
		replay:   true,
		recorded: cfg.Calls,
		queues:   map[string][]int{},
		used:     make([]bool, len(cfg.Calls)),
		strict:   cfg.Strict,
	}
	for i, c := range cfg.Calls {
		k := callKey(c)
		p.queues[k] = append(p.queues[k], i)
	}
	return p, p.listen()
}

func (p *Proxy) listen() error {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	p.ln = ln
	p.start = time.Now()
	p.drained = make(chan struct{})
	p.conns = map[net.Conn]struct{}{}
	p.srv = &http.Server{Handler: http.HandlerFunc(p.serveProxy), ReadHeaderTimeout: 30 * time.Second, ErrorLog: quietLog()}
	p.tunnels = &http.Server{
		Handler:           http.HandlerFunc(p.serveTunnel),
		ReadHeaderTimeout: 30 * time.Second,
		ErrorLog:          quietLog(),
		ConnState: func(c net.Conn, s http.ConnState) {
			if s == http.StateClosed || s == http.StateHijacked {
				p.untrack(c)
			}
		},
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if t, ok := c.(*tunnelConn); ok {
				return context.WithValue(ctx, hostKey{}, t.host)
			}
			return ctx
		},
	}
	go p.srv.Serve(ln)
	return nil
}

// URL is the proxy's address, for HTTP_PROXY and HTTPS_PROXY.
func (p *Proxy) URL() string { return "http://" + p.ln.Addr().String() }

// Close stops the proxy and drops open connections, then waits up to five
// seconds for exchanges in progress to be noted. It can be called more than
// once.
func (p *Proxy) Close() error {
	p.mu.Lock()
	first := !p.closing
	p.closing = true
	if first && p.inflight == 0 {
		close(p.drained)
	}
	p.mu.Unlock()
	p.connsMu.Lock()
	p.closed = true
	for c := range p.conns {
		c.Close()
	}
	p.connsMu.Unlock()
	err := p.srv.Close()
	p.tunnels.Close()
	if p.transport != nil {
		p.transport.CloseIdleConnections()
	}
	select {
	case <-p.drained:
	case <-time.After(5 * time.Second):
	}
	return err
}

// Calls returns the recorded exchanges in the order their requests arrived.
func (p *Proxy) Calls() []instance.Call {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := append([]instance.Call(nil), p.calls...)
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out
}

// Served is how many requests a replay answered from the recording.
func (p *Proxy) Served() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.served
}

// Divergences lists how a replay's traffic differed from the recording:
// requests with no recorded answer, and recorded requests never made.
func (p *Proxy) Divergences() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := append([]string(nil), p.divergences...)
	for i, used := range p.used {
		if !used {
			c := p.recorded[i]
			out = append(out, fmt.Sprintf("recorded call %d (%s %s) was never made", c.Seq, c.Request.Method, c.Request.URL))
		}
	}
	return out
}

func (p *Proxy) serveProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.connect(w, r)
		return
	}
	if !r.URL.IsAbs() || r.URL.Host == "" {
		http.Error(w, "codeinstances: this is a proxy; send it proxy requests", http.StatusBadRequest)
		return
	}
	u := *r.URL
	u.Host = canonicalHost(u.Scheme, u.Host)
	p.exchange(w, r, u.String())
}

type hostKey struct{}

func (p *Proxy) serveTunnel(w http.ResponseWriter, r *http.Request) {
	host, _ := r.Context().Value(hostKey{}).(string)
	u := url.URL{Scheme: "https", Host: canonicalHost("https", host), Path: r.URL.Path, RawPath: r.URL.RawPath, RawQuery: r.URL.RawQuery}
	if u.Path == "" {
		u.Path = "/"
	}
	p.exchange(w, r, u.String())
}

// connect answers CONNECT host:port and serves HTTPS on the tunnel, with a
// certificate for that host from the run's authority.
func (p *Proxy) connect(w http.ResponseWriter, r *http.Request) {
	target := r.Host
	host, _, err := net.SplitHostPort(target)
	if err != nil {
		host = target
		target = net.JoinHostPort(target, "443")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "codeinstances: can't take over the connection", http.StatusInternalServerError)
		return
	}
	conn, buf, err := hj.Hijack()
	if err != nil {
		return
	}
	if _, err := conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		conn.Close()
		return
	}
	var raw net.Conn = conn
	if buf != nil && buf.Reader.Buffered() > 0 {
		raw = &prefixConn{Conn: conn, r: io.MultiReader(io.LimitReader(buf.Reader, int64(buf.Reader.Buffered())), conn)}
	}
	tlsConn := tls.Server(raw, &tls.Config{
		GetCertificate: p.auth.GetCertificate(host),
		NextProtos:     []string{"http/1.1"},
		MinVersion:     tls.VersionTLS12,
	})
	tc := &tunnelConn{Conn: tlsConn, host: target}
	if !p.track(tc) {
		tc.Close()
		return
	}
	go func() {
		p.tunnels.Serve(&oneConnListener{conn: tc, addr: p.ln.Addr()})
	}()
}

func (p *Proxy) track(c net.Conn) bool {
	p.connsMu.Lock()
	defer p.connsMu.Unlock()
	if p.closed {
		return false
	}
	p.conns[c] = struct{}{}
	return true
}

func (p *Proxy) untrack(c net.Conn) {
	p.connsMu.Lock()
	delete(p.conns, c)
	p.connsMu.Unlock()
}

func (p *Proxy) exchange(w http.ResponseWriter, r *http.Request, target string) {
	p.mu.Lock()
	if p.closing {
		p.mu.Unlock()
		http.Error(w, "codeinstances: the run is over", http.StatusServiceUnavailable)
		return
	}
	p.inflight++
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		p.inflight--
		if p.inflight == 0 && p.closing {
			close(p.drained)
		}
		p.mu.Unlock()
	}()
	at := time.Since(p.start).Milliseconds()
	body, err := readLimited(r.Body)
	tooLarge := err != nil
	if p.replay {
		k := key(r.Method, target, body)
		if tooLarge {
			k = tooLargeKey(r.Method, target)
		}
		p.answer(w, r.Method, target, k)
		return
	}
	if tooLarge {
		// Kept as a call that failed, so a replay fails it the same way.
		p.mu.Lock()
		p.seq++
		call := instance.Call{Seq: p.seq, Kind: instance.KindHTTP, AtMS: at, Error: ErrRequestTooLarge,
			Request: instance.Request{Method: r.Method, URL: target, Header: p.keep(passable(r.Header))}}
		p.calls = append(p.calls, call)
		p.mu.Unlock()
		writeError(w, call.Error)
		return
	}
	p.forward(w, r, target, body, at)
}

// ErrRequestTooLarge is the error kept for a request whose body was over
// MaxBody. Its body isn't kept, and replay matches it by method and URL.
var ErrRequestTooLarge = fmt.Sprintf("the request body was bigger than %d MiB, so it wasn't sent", MaxBody>>20)

func tooLargeKey(method, target string) string { return method + " " + target + " too-large" }

func callKey(c instance.Call) string {
	if c.Error == ErrRequestTooLarge {
		return tooLargeKey(c.Request.Method, c.Request.URL)
	}
	return key(c.Request.Method, c.Request.URL, c.Request.Body.MustBytes())
}

// forward passes the request on, keeps the exchange, and returns the answer.
func (p *Proxy) forward(w http.ResponseWriter, r *http.Request, target string, body []byte, at int64) {
	p.mu.Lock()
	p.seq++
	call := instance.Call{Seq: p.seq, Kind: instance.KindHTTP, AtMS: at}
	p.mu.Unlock()
	call.Request = instance.Request{Method: r.Method, URL: target, Header: p.keep(passable(r.Header)), Body: instance.NewBody(body)}
	defer func() {
		p.mu.Lock()
		p.calls = append(p.calls, call)
		p.mu.Unlock()
	}()

	if r.Header.Get("Upgrade") != "" {
		call.Error = "protocol upgrades (WebSocket) aren't recorded in this version"
		writeError(w, call.Error)
		return
	}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		call.Error = err.Error()
		writeError(w, call.Error)
		return
	}
	out.Header = passable(r.Header)
	// The body is already here, so there is nothing to wait for.
	out.Header.Del("Expect")
	out.Host = r.Host
	if _, ok := out.Header["User-Agent"]; !ok {
		// An empty value stops Go adding its own User-Agent.
		out.Header["User-Agent"] = []string{""}
	}
	resp, err := p.transport.RoundTrip(out)
	if err != nil {
		call.Error = err.Error()
		writeError(w, call.Error)
		return
	}
	defer resp.Body.Close()
	respBody, err := readLimited(resp.Body)
	if err != nil {
		call.Error = "response body: " + err.Error()
		writeError(w, call.Error)
		return
	}
	header := passable(resp.Header)
	call.Response = &instance.Response{Status: resp.StatusCode, Header: p.keep(header), Body: instance.NewBody(respBody)}
	writeResponse(w, r.Method, resp.StatusCode, header, respBody)
}

// answer serves a replayed request from the recording.
func (p *Proxy) answer(w http.ResponseWriter, method, target, k string) {
	p.mu.Lock()
	p.requests++
	idx := -1
	if p.strict {
		if p.next < len(p.recorded) {
			c := p.recorded[p.next]
			if callKey(c) == k {
				idx = p.next
				p.next++
			} else {
				p.divergences = append(p.divergences, fmt.Sprintf("request %d was %s %s, but the next in the recording is %s %s", p.requests, method, target, c.Request.Method, c.Request.URL))
			}
		} else {
			p.divergences = append(p.divergences, fmt.Sprintf("extra request %s %s after the %d recorded", method, target, len(p.recorded)))
		}
	} else {
		q := p.queues[k]
		if len(q) > 0 {
			idx = q[0]
			p.queues[k] = q[1:]
		} else {
			p.divergences = append(p.divergences, fmt.Sprintf("request %s %s isn't in the recording%s", method, target, hint(p.recorded, method, target)))
		}
	}
	if idx >= 0 {
		p.used[idx] = true
		p.served++
	}
	p.mu.Unlock()

	if idx < 0 {
		w.Header().Set("X-Codeinstances", "unmatched")
		http.Error(w, "codeinstances: no recorded response for "+method+" "+target, http.StatusBadGateway)
		return
	}
	c := p.recorded[idx]
	if c.Error != "" {
		writeError(w, c.Error)
		return
	}
	writeResponse(w, method, c.Response.Status, c.Response.Header, c.Response.Body.MustBytes())
}

// hint says when a request matches a recorded one except for its body, the
// commonest near miss.
func hint(calls []instance.Call, method, target string) string {
	for _, c := range calls {
		if c.Request.Method == method && c.Request.URL == target {
			return " (one with the same method and URL is, with a different body)"
		}
	}
	return ""
}

func writeError(w http.ResponseWriter, msg string) {
	w.Header().Set("X-Codeinstances", "upstream-error")
	http.Error(w, "codeinstances: upstream error: "+msg, http.StatusBadGateway)
}

func writeResponse(w http.ResponseWriter, method string, status int, header map[string][]string, body []byte) {
	h := w.Header()
	for k, vs := range header {
		if strings.EqualFold(k, "Content-Length") {
			continue
		}
		for _, v := range vs {
			h.Add(k, v)
		}
	}
	switch {
	case method == http.MethodHead:
		// A HEAD response carries the length of the body it doesn't send.
		for k, vs := range header {
			if strings.EqualFold(k, "Content-Length") && len(vs) > 0 {
				h.Set("Content-Length", vs[0])
			}
		}
	case status >= 200 && status != http.StatusNoContent && status != http.StatusNotModified:
		h.Set("Content-Length", fmt.Sprint(len(body)))
	}
	w.WriteHeader(status)
	if method != http.MethodHead && len(body) > 0 {
		w.Write(body)
	}
}

// keep copies a header for the instance file, redacting secrets.
func (p *Proxy) keep(h http.Header) map[string][]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string][]string, len(h))
	for k, vs := range h {
		vs = append([]string(nil), vs...)
		if !p.keepSecrets && isSecret(k) {
			for i := range vs {
				vs[i] = Redacted
			}
		}
		out[k] = vs
	}
	return out
}

func isSecret(name string) bool {
	for _, s := range SecretHeaders {
		if strings.EqualFold(name, s) {
			return true
		}
	}
	return false
}

// passable drops hop-by-hop headers, including any the Connection header
// names.
func passable(h http.Header) http.Header {
	out := h.Clone()
	for _, f := range h.Values("Connection") {
		for _, name := range strings.Split(f, ",") {
			if name = strings.TrimSpace(name); name != "" {
				out.Del(name)
			}
		}
	}
	for _, k := range hopHeaders {
		out.Del(k)
	}
	return out
}

func readLimited(r io.Reader) ([]byte, error) {
	if r == nil {
		return nil, nil
	}
	b, err := io.ReadAll(io.LimitReader(r, MaxBody+1))
	if err != nil {
		return nil, err
	}
	if len(b) > MaxBody {
		return nil, fmt.Errorf("bigger than %d MiB", MaxBody>>20)
	}
	return b, nil
}

// key is how replay finds the answer to a request: method, URL and a hash of
// the body.
func key(method, target string, body []byte) string {
	sum := sha256.Sum256(body)
	return method + " " + target + " " + hex.EncodeToString(sum[:])
}

// canonicalHost lowercases the host and drops the scheme's default port, so
// "Example.com:443" and "example.com" are one URL.
func canonicalHost(scheme, host string) string {
	host = strings.ToLower(host)
	if h, port, err := net.SplitHostPort(host); err == nil {
		if (scheme == "https" && port == "443") || (scheme == "http" && port == "80") {
			if strings.Contains(h, ":") {
				return "[" + h + "]"
			}
			return h
		}
	}
	return host
}

func resolveAddr(resolve map[string]string, addr string) string {
	addr = strings.ToLower(addr)
	if to, ok := resolve[addr]; ok {
		return to
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if to, ok := resolve[host]; ok {
		if _, _, err := net.SplitHostPort(to); err == nil {
			return to
		}
		return net.JoinHostPort(to, port)
	}
	return addr
}

func hostPort(u *url.URL) string {
	if u.Port() != "" {
		return strings.ToLower(u.Host)
	}
	port := "80"
	if u.Scheme == "https" {
		port = "443"
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// tunnelConn carries the CONNECT target with the decrypted connection.
type tunnelConn struct {
	net.Conn
	host string
}

// prefixConn replays bytes the HTTP server had already buffered.
type prefixConn struct {
	net.Conn
	r io.Reader
}

func (c *prefixConn) Read(b []byte) (int, error) { return c.r.Read(b) }

// oneConnListener hands one connection to an http.Server.
type oneConnListener struct {
	mu   sync.Mutex
	conn net.Conn
	addr net.Addr
}

func (l *oneConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conn == nil {
		return nil, io.EOF
	}
	c := l.conn
	l.conn = nil
	return c, nil
}

func (l *oneConnListener) Close() error   { return nil }
func (l *oneConnListener) Addr() net.Addr { return l.addr }

func quietLog() *log.Logger { return log.New(io.Discard, "", 0) }
