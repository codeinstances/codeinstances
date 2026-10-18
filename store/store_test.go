// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codeinstances/codeinstances"
	"github.com/codeinstances/codeinstances/instance"
)

func sample(t *testing.T, n int, exit int, stderr string) []byte {
	t.Helper()
	inst := &instance.Instance{Tool: "test", Created: time.Date(2026, 10, 18, 6, n, 0, 0, time.UTC),
		Command: instance.Command{Argv: []string{"prog", fmt.Sprint(n)}},
		Result:  instance.Result{ExitCode: exit, Stderr: instance.NewBody([]byte(stderr))}}
	data, err := instance.Marshal(inst)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func idOf(t *testing.T, data []byte) string {
	inst, err := instance.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return inst.ID
}

func TestStore(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "store")
	s, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	a, b := sample(t, 1, 0, ""), sample(t, 2, 1, "request 41 timed out\n")
	id, created, err := s.Put(a)
	if err != nil || !created || id != idOf(t, a) {
		t.Fatal(id, created, err)
	}
	if _, created, _ := s.Put(a); created {
		t.Fatal("stored twice")
	}
	s.Put(b)
	s.Put(sample(t, 3, 1, "request 97 timed out\n"))
	if _, _, err := s.Put([]byte(`{"hello":1}`)); err == nil {
		t.Fatal("took a file that isn't an instance")
	}
	edited := bytes.Replace(b, []byte(`"exit_code": 1`), []byte(`"exit_code": 0`), 1)
	if _, _, err := s.Put(edited); !errors.Is(err, instance.ErrModified) {
		t.Fatal(err)
	}

	hex := strings.TrimPrefix(id, "sha256:")
	for _, ref := range []string{id, hex, hex[:12], strings.ToUpper(hex[:20]), " sha256:" + hex[:16] + " "} {
		got, err := s.Resolve(ref)
		if err != nil || got != id {
			t.Errorf("%q: %v %v", ref, got, err)
		}
	}
	for ref, want := range map[string]error{hex[:11]: ErrBadRef, "zz" + hex[:20]: ErrBadRef, strings.Repeat("0", 64): ErrNotFound, "000000000000": ErrNotFound} {
		if _, err := s.Resolve(ref); !errors.Is(err, want) {
			t.Errorf("%q: %v, want %v", ref, err, want)
		}
	}
	data, gotID, err := s.Get(hex[:12])
	if err != nil || gotID != id || !bytes.Equal(data, a) {
		t.Fatal(err)
	}

	entries := s.List()
	if len(entries) != 3 || entries[0].Command[1] != "3" || entries[2].ID != id {
		t.Fatalf("%+v", entries)
	}
	if entries[2].Failed || entries[2].Fingerprint != "" || !entries[0].Failed || !strings.HasPrefix(entries[0].Fingerprint, "fp1:") {
		t.Fatalf("%+v", entries)
	}
	g := s.Groups()
	if g.Failing != 2 || g.Passed != 1 || len(g.Groups) != 1 || len(g.Groups[0].Runs) != 2 {
		t.Fatalf("%+v", g)
	}

	// Opened again, the store reads what is in the folder, leaving out stray
	// files, damaged ones, files under the wrong id and symlinks.
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(dir, strings.Repeat("a", 64)+".instance"), []byte("broken"), 0o600)
	os.WriteFile(filepath.Join(dir, strings.Repeat("b", 64)+".instance"), a, 0o600)
	if err := os.Symlink(filepath.Join(dir, hex+".instance"), filepath.Join(dir, strings.Repeat("c", 64)+".instance")); err != nil {
		t.Log("no symlinks here:", err)
	}
	s2, err := Open(dir)
	if err != nil || len(s2.List()) != 3 {
		t.Fatalf("%v %+v", err, s2.List())
	}
	for _, ref := range []string{strings.Repeat("a", 12), strings.Repeat("b", 12), strings.Repeat("c", 12)} {
		if _, _, err := s2.Get(ref); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: %v", ref, err)
		}
	}

	// A stored file damaged on disk isn't served, and the next push of the
	// instance puts it right.
	os.WriteFile(filepath.Join(dir, hex+".instance"), []byte("damaged"), 0o600)
	if _, _, err := s2.Get(id); !errors.Is(err, ErrDamaged) {
		t.Fatal(err)
	}
	if _, created, err := s2.Put(a); err != nil || !created {
		t.Fatal(created, err)
	}
	if data, _, err := s2.Get(id); err != nil || !bytes.Equal(data, a) {
		t.Fatal(err)
	}
}

func TestAmbiguousPrefix(t *testing.T) {
	s, _ := Open(t.TempDir())
	// Two ids that share the first 12 digits can't be made on purpose, so
	// the index is filled by hand.
	for _, suffix := range []string{"1", "2"} {
		s.index["abcdefabcdef"+strings.Repeat("0", 51)+suffix] = indexed{}
	}
	if _, err := s.Resolve("abcdefabcdef"); !errors.Is(err, ErrAmbiguous) {
		t.Fatal(err)
	}
}

func do(t *testing.T, h http.Handler, method, path, token string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return w
}

func TestServer(t *testing.T) {
	s, _ := Open(t.TempDir())
	srv := &Server{Store: s, Token: "sesame", MaxBytes: 4096}
	a := sample(t, 1, 1, "panic: <script>alert(1)</script>\n")

	if w := do(t, srv, "POST", "/v1/instances", "", a); w.Code != 401 || w.Header().Get("WWW-Authenticate") == "" {
		t.Fatal(w.Code)
	}
	if w := do(t, srv, "POST", "/v1/instances", "wrong", a); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := do(t, srv, "GET", "/v1/instances", "", nil); w.Code != 401 {
		t.Fatal("read without the token")
	}
	w := do(t, srv, "POST", "/v1/instances", "sesame", a)
	var p Pushed
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &p) != nil || p.ID != idOf(t, a) || !p.Created || p.Path != "/v1/instances/"+p.ID || strings.Contains(w.Body.String(), "example.com") {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if w := do(t, srv, "POST", "/v1/instances/", "sesame", a); w.Code != 200 {
		t.Fatal("a second push", w.Code)
	}
	if w := do(t, srv, "POST", "/v1/instances", "sesame", bytes.Replace(a, []byte(`"exit_code": 1`), []byte(`"exit_code": 2`), 1)); w.Code != 422 {
		t.Fatal("edited", w.Code, w.Body)
	}
	if w := do(t, srv, "POST", "/v1/instances", "sesame", []byte("nope")); w.Code != 400 {
		t.Fatal(w.Code)
	}
	if w := do(t, srv, "POST", "/v1/instances", "sesame", bytes.Repeat([]byte(" "), 5000)); w.Code != 413 {
		t.Fatal("too big", w.Code)
	}
	// A form post, as a web page could send without asking, is refused.
	req := httptest.NewRequest("POST", "/v1/instances", bytes.NewReader(a))
	req.Header.Set("Authorization", "Bearer sesame")
	req.Header.Set("Content-Type", "text/plain")
	if w := httptest.NewRecorder(); func() int { srv.ServeHTTP(w, req); return w.Code }() != 415 {
		t.Fatal("text/plain upload")
	}
	// The scheme is case-insensitive.
	req = httptest.NewRequest("GET", "/v1/instances", nil)
	req.Header.Set("Authorization", "bearer sesame")
	if w := httptest.NewRecorder(); func() int { srv.ServeHTTP(w, req); return w.Code }() != 200 {
		t.Fatal("lowercase bearer")
	}
	// Uploads past the limit at once are turned away.
	for i := 0; i < DefaultMaxUploads; i++ {
		srv.slots() <- struct{}{}
	}
	if w := do(t, srv, "POST", "/v1/instances", "sesame", a); w.Code != 503 || w.Header().Get("Retry-After") == "" {
		t.Fatal("busy", w.Code)
	}
	for i := 0; i < DefaultMaxUploads; i++ {
		<-srv.slots()
	}
	if w := do(t, srv, "DELETE", "/v1/instances", "sesame", nil); w.Code != 405 || w.Header().Get("Allow") != "GET, HEAD, POST" {
		t.Fatal("delete", w.Code)
	}
	for _, m := range []string{"T", "OST", "GE"} {
		if w := do(t, srv, m, "/v1/instances", "sesame", nil); w.Code != 405 {
			t.Errorf("%s: %d", m, w.Code)
		}
	}
	if w := do(t, srv, "HEAD", "/v1/groups", "sesame", nil); w.Code != 200 {
		t.Fatal("head", w.Code)
	}
	s.Put(sample(t, 2, 0, ""))

	w = do(t, srv, "GET", "/v1/instances/"+strings.TrimPrefix(p.ID, "sha256:")[:14], "sesame", nil)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), a) || w.Header().Get("ETag") != `"`+p.ID+`"` || !strings.Contains(w.Header().Get("Content-Disposition"), instance.ShortID(p.ID)+".instance") {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
	for path, code := range map[string]int{"/v1/instances/xyz": 400, "/v1/instances/" + strings.Repeat("0", 64): 404, "/nothing": 404} {
		if w := do(t, srv, "GET", path, "sesame", nil); w.Code != code {
			t.Errorf("%s: %d, want %d", path, w.Code, code)
		}
	}
	var list struct{ Instances []Entry }
	w = do(t, srv, "GET", "/v1/instances?failed=true", "sesame", nil)
	if json.Unmarshal(w.Body.Bytes(), &list) != nil || len(list.Instances) != 1 || list.Instances[0].ID != p.ID {
		t.Fatal(w.Body)
	}
	w = do(t, srv, "GET", "/v1/groups", "sesame", nil)
	if !strings.Contains(w.Body.String(), `"failing": 1`) || !strings.Contains(w.Body.String(), "panic: <script>") {
		t.Fatal(w.Body)
	}
	w = do(t, srv, "GET", "/", "sesame", nil)
	page := w.Body.String()
	if w.Code != 200 || strings.Contains(page, "<script>") || !strings.Contains(page, "&lt;script&gt;") || !strings.Contains(page, "2 instances, 1 of them failing, in 1 groups") ||
		!strings.Contains(w.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("%d %s", w.Code, page)
	}

	// With OpenRead, reads need no token; uploads still do.
	srv.OpenRead = true
	if w := do(t, srv, "GET", "/v1/instances", "", nil); w.Code != 200 {
		t.Fatal(w.Code)
	}
	if w := do(t, srv, "POST", "/v1/instances", "", a); w.Code != 401 {
		t.Fatal(w.Code)
	}
}

func TestClient(t *testing.T) {
	s, _ := Open(t.TempDir())
	ts := httptest.NewServer(&Server{Store: s, Token: "sesame"})
	defer ts.Close()
	c := &Client{URL: ts.URL + "/", Token: "sesame"}
	a := sample(t, 1, 1, "boom\n")
	p, err := c.Push(a)
	if err != nil || !p.Created || p.URL != ts.URL+"/v1/instances/"+p.ID {
		t.Fatal(err, p)
	}
	data, inst, err := c.Pull(strings.TrimPrefix(p.ID, "sha256:")[:12])
	if err != nil || !bytes.Equal(data, a) || inst.ID != p.ID {
		t.Fatal(err)
	}
	if p, err := c.Push(a); err != nil || p.Created {
		t.Fatal("second push", err)
	}
	if _, err := c.Push([]byte("nope")); err == nil {
		t.Fatal("pushed a file that isn't an instance")
	}
	if _, _, err := c.Pull(strings.Repeat("1", 64)); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatal(err)
	}
	if _, err := (&Client{URL: ts.URL}).Push(a); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatal(err)
	}
	if _, err := (&Client{URL: "ftp://x"}).Push(a); err == nil {
		t.Fatal("ftp")
	}

	// A store that sends something else is caught.
	liar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, string(sample(t, 9, 0, "")))
	}))
	defer liar.Close()
	if _, _, err := (&Client{URL: liar.URL}).Pull(p.ID); err == nil || !strings.Contains(err.Error(), "not "+p.ID) {
		t.Fatal(err)
	}
}

func TestHostsAndRedirects(t *testing.T) {
	s, _ := Open(t.TempDir())
	srv := &Server{Store: s, Token: "sesame", OpenRead: true, Hosts: []string{"127.0.0.1", "localhost", "::1"}}
	for host, code := range map[string]int{"127.0.0.1:7979": 200, "localhost": 200, "[::1]:7979": 200, "LOCALHOST:1": 200, "evil.example:7979": 421, "127.0.0.2": 421} {
		req := httptest.NewRequest("GET", "/v1/instances", nil)
		req.Host = host
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, req)
		if w.Code != code {
			t.Errorf("%s: %d, want %d", host, w.Code, code)
		}
	}
	// The client follows no redirect, so the token stays where it was sent.
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked = r.Header.Get("Authorization") != "" }))
	defer other.Close()
	mover := httptest.NewServer(http.RedirectHandler(other.URL+"/v1/instances", http.StatusTemporaryRedirect))
	defer mover.Close()
	_, err := (&Client{URL: mover.URL, Token: "sesame"}).Push(sample(t, 1, 0, ""))
	if err == nil || !strings.Contains(err.Error(), "redirect") || leaked {
		t.Fatal(err, leaked)
	}
	// A server that answers with another id is caught.
	odd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"id":"sha256:`+strings.Repeat("0", 64)+`","path":"/x","created":true}`)
	}))
	defer odd.Close()
	if _, err := (&Client{URL: odd.URL}).Push(sample(t, 1, 0, "")); err == nil || !strings.Contains(err.Error(), "another id") {
		t.Fatal(err)
	}
}

func TestTrustedServer(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	key, _ := codeinstances.GenerateKey()
	other, _ := codeinstances.GenerateKey()
	srv := &Server{Store: s, Token: "sesame", Trusted: []codeinstances.TrustedKey{{Key: key.Public().(ed25519.PublicKey), Label: "ci"}}}
	signed := func(k ed25519.PrivateKey, n int) []byte {
		inst := &instance.Instance{Tool: "test", Created: time.Date(2026, 10, 18, 6, n, 0, 0, time.UTC),
			Command: instance.Command{Argv: []string{"prog", fmt.Sprint(n)}}}
		if k != nil {
			if err := inst.Sign(k, "ci", time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		data, err := instance.Marshal(inst)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if w := do(t, srv, "POST", "/v1/instances", "sesame", signed(nil, 1)); w.Code != 403 || !strings.Contains(w.Body.String(), "signed by a key it trusts") {
		t.Fatalf("unsigned: %d %s", w.Code, w.Body)
	}
	if w := do(t, srv, "POST", "/v1/instances", "sesame", signed(other, 2)); w.Code != 403 {
		t.Fatalf("untrusted key: %d", w.Code)
	}
	good := signed(key, 3)
	if w := do(t, srv, "POST", "/v1/instances", "sesame", good); w.Code != 201 {
		t.Fatalf("trusted: %d %s", w.Code, w.Body)
	}
	// A signature that doesn't check out is refused whether or not the
	// server trusts anyone.
	bad := bytes.Replace(good, []byte(`"prog"`), []byte(`"prug"`), 1)
	inst, _ := instance.Parse(good)
	newID, _ := instance.ComputeID(bad)
	bad = bytes.Replace(bad, []byte(inst.ID), []byte(newID), 1)
	for _, h := range []*Server{srv, {Store: s, Token: "sesame"}} {
		if w := do(t, h, "POST", "/v1/instances", "sesame", bad); w.Code != 422 || !strings.Contains(w.Body.String(), "signature") {
			t.Fatalf("bad signature: %d %s", w.Code, w.Body)
		}
	}
	entries := s.List()
	if len(entries) != 1 || len(entries[0].SignedBy) != 1 || entries[0].SignedBy[0] != instance.KeyID(key.Public().(ed25519.PublicKey)) {
		t.Fatalf("%+v", entries)
	}
	if w := do(t, srv, "GET", "/", "sesame", nil); !strings.Contains(w.Body.String(), entries[0].SignedBy[0]) {
		t.Fatal("the page doesn't show the signing key")
	}
}
