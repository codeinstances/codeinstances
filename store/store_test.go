// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
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
	// A stray file in the folder is ignored.
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(dir, strings.Repeat("a", 64)+".instance"), []byte("broken"), 0o600)

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

	entries, err := s.List()
	if err != nil || len(entries) != 3 || entries[0].Command[1] != "3" || entries[2].ID != id {
		t.Fatalf("%v %+v", err, entries)
	}
	if entries[2].Failed || entries[2].Fingerprint != "" || !entries[0].Failed || !strings.HasPrefix(entries[0].Fingerprint, "fp1:") {
		t.Fatalf("%+v", entries)
	}
	g, err := s.Groups()
	if err != nil || g.Failing != 2 || g.Passed != 1 || len(g.Groups) != 1 || len(g.Groups[0].Runs) != 2 {
		t.Fatalf("%v %+v", err, g)
	}
}

func TestAmbiguousPrefix(t *testing.T) {
	s, _ := Open(t.TempDir())
	// Two ids that share the first 12 digits can't be made on purpose, so
	// the files are named by hand.
	for _, suffix := range []string{"1", "2"} {
		os.WriteFile(filepath.Join(s.Dir(), "abcdefabcdef"+strings.Repeat("0", 51)+suffix+".instance"), nil, 0o600)
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
	if w.Code != 201 || json.Unmarshal(w.Body.Bytes(), &p) != nil || p.ID != idOf(t, a) || !p.Created || p.URL != "http://example.com/v1/instances/"+p.ID {
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
