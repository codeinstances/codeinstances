// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"mime"
	"net"
	"net/http"
	"os"
	"slices"
	"strings"
	"sync"

	"github.com/codeinstances/codeinstances"
	"github.com/codeinstances/codeinstances/instance"
)

// DefaultMaxBytes is the largest instance a server takes by default.
const DefaultMaxBytes = 64 << 20

// DefaultMaxUploads is how many uploads a server reads at once by default.
const DefaultMaxUploads = 2

// Server serves a store over HTTP:
//
//	GET  /                      a page listing the groups and the latest instances
//	POST /v1/instances          store the instance in the request body (Content-Type: application/json)
//	GET  /v1/instances          list the stored instances as JSON (?failed=true for failing ones)
//	GET  /v1/instances/{id}     download an instance by id or unique prefix
//	GET  /v1/groups             the failing instances grouped by fingerprint, as JSON
//	POST /v1/instances/{id}/replay  replay the instance offline (with a Runner)
//	POST /v1/instances/{id}/fork    fork it offline at a step (with a Runner)
//
// HEAD works wherever GET does.
type Server struct {
	Store *Store
	// Token, when set, must be sent as "Authorization: Bearer <token>".
	// It's needed to upload, and to read too unless OpenRead is set.
	Token    string
	OpenRead bool
	// MaxBytes limits an upload; zero means DefaultMaxBytes.
	MaxBytes int64
	// MaxUploads limits uploads read at once; zero means DefaultMaxUploads.
	// More get 503.
	MaxUploads int
	// Hosts, when set, are the only host names a request may name in its
	// Host header. A server on a loopback address sets them to its loopback
	// names, so a web page can't reach it through DNS rebinding.
	Hosts []string
	// Trusted, when set, makes the server take only instances signed by one
	// of these keys. Instances already in the folder are served as they are.
	Trusted []codeinstances.TrustedKey
	// Runner, when set, lets the server run the instances it keeps; see
	// Runner. It runs only instances signed by a key in Trusted.
	Runner *Runner

	slotsOnce sync.Once
	uploads   chan struct{}
}

func (s *Server) maxBytes() int64 {
	if s.MaxBytes > 0 {
		return s.MaxBytes
	}
	return DefaultMaxBytes
}

func (s *Server) slots() chan struct{} {
	s.slotsOnce.Do(func() {
		n := s.MaxUploads
		if n <= 0 {
			n = DefaultMaxUploads
		}
		s.uploads = make(chan struct{}, n)
	})
	return s.uploads
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	if !s.hostAllowed(r.Host) {
		problem(w, http.StatusMisdirectedRequest, "this store doesn't answer to that host name")
		return
	}
	path := strings.TrimSuffix(r.URL.Path, "/")
	method := r.Method
	if method == http.MethodHead {
		method = http.MethodGet
	}
	var allow, runKind, runRef string
	if rest, ok := strings.CutPrefix(path, "/v1/instances/"); ok && s.Runner != nil {
		if ref, kind, ok := strings.Cut(rest, "/"); ok && (kind == "replay" || kind == "fork") {
			runRef, runKind = ref, kind
		}
	}
	switch {
	case runKind != "":
		allow = "POST"
	case path == "" || path == "/v1/groups" || strings.HasPrefix(path, "/v1/instances/") && !strings.Contains(strings.TrimPrefix(path, "/v1/instances/"), "/"):
		allow = "GET, HEAD"
	case path == "/v1/instances":
		allow = "GET, HEAD, POST"
	default:
		problem(w, http.StatusNotFound, "no such page")
		return
	}
	if !slices.Contains(strings.Split(allow, ", "), method) {
		w.Header().Set("Allow", allow)
		problem(w, http.StatusMethodNotAllowed, "use "+allow)
		return
	}
	if !s.allowed(r, method == http.MethodPost) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="codeinstances"`)
		problem(w, http.StatusUnauthorized, "a token is needed: send Authorization: Bearer <token>")
		return
	}
	switch {
	case runKind != "":
		if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
			problem(w, http.StatusUnsupportedMediaType, "send the request with Content-Type: application/json")
			return
		}
		s.run(w, r, runRef, runKind)
	case path == "":
		s.index(w)
	case path == "/v1/instances" && method == http.MethodPost:
		s.upload(w, r)
	case path == "/v1/instances":
		s.list(w, r)
	case path == "/v1/groups":
		writeJSON(w, http.StatusOK, s.Store.Groups())
	default:
		s.download(w, strings.TrimPrefix(path, "/v1/instances/"))
	}
}

func (s *Server) hostAllowed(host string) bool {
	if len(s.Hosts) == 0 {
		return true
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	host = strings.Trim(strings.ToLower(host), "[]")
	for _, h := range s.Hosts {
		if host == strings.Trim(strings.ToLower(h), "[]") {
			return true
		}
	}
	return false
}

func (s *Server) allowed(r *http.Request, write bool) bool {
	if s.Token == "" || !write && s.OpenRead {
		return true
	}
	scheme, got, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	return ok && strings.EqualFold(scheme, "Bearer") && subtle.ConstantTimeCompare([]byte(strings.TrimSpace(got)), []byte(s.Token)) == 1
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
	// A browser can't send this content type to another site without asking
	// first, so a web page can't plant instances in the store.
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "application/json" {
		problem(w, http.StatusUnsupportedMediaType, "send the instance with Content-Type: application/json")
		return
	}
	select {
	case s.slots() <- struct{}{}:
		defer func() { <-s.slots() }()
	default:
		w.Header().Set("Retry-After", "5")
		problem(w, http.StatusServiceUnavailable, "the store is busy with other uploads; try again")
		return
	}
	max := s.MaxBytes
	if max <= 0 {
		max = DefaultMaxBytes
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, max))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			problem(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the instance is over %d bytes", max))
			return
		}
		problem(w, http.StatusBadRequest, "the upload was cut short")
		return
	}
	if len(s.Trusted) > 0 {
		inst, err := instance.Parse(data)
		if err == nil {
			_, err = codeinstances.CheckSignatures(inst, s.Trusted)
		}
		if errors.Is(err, codeinstances.ErrNotTrusted) {
			problem(w, http.StatusForbidden, "the store takes only instances signed by a key it trusts")
			return
		}
	}
	id, created, err := s.Store.Put(data)
	if err != nil {
		if errors.Is(err, instance.ErrModified) || errors.Is(err, instance.ErrBadSignature) {
			problem(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		var fsErr *fs.PathError
		var linkErr *os.LinkError
		if errors.As(err, &fsErr) || errors.As(err, &linkErr) {
			problem(w, http.StatusInternalServerError, "the store couldn't keep the file")
			return
		}
		problem(w, http.StatusBadRequest, err.Error())
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, Pushed{ID: id, Path: "/v1/instances/" + id, Created: created})
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	entries := s.Store.List()
	if r.URL.Query().Get("failed") == "true" {
		kept := entries[:0]
		for _, e := range entries {
			if e.Failed {
				kept = append(kept, e)
			}
		}
		entries = kept
	}
	writeJSON(w, http.StatusOK, map[string]any{"instances": entries})
}

func (s *Server) download(w http.ResponseWriter, ref string) {
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
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", `"`+id+`"`)
	w.Header().Set("Content-Disposition", `attachment; filename="`+instance.ShortID(id)+`.instance"`)
	w.Write(data)
}

var page = template.Must(template.New("index").Funcs(template.FuncMap{
	"short": instance.ShortID,
	"join":  func(a []string) string { return strings.Join(a, " ") },
	"last":  func(a []string) int { return len(a) - 1 },
}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>CodeInstances store</title>
<style>
body{font:15px/1.5 system-ui,sans-serif;margin:2rem auto;max-width:72rem;padding:0 1rem;color:#17212e}
h1{font-size:1.4rem}h2{font-size:1.1rem;margin-top:2rem}
table{border-collapse:collapse;width:100%}td,th{text-align:left;padding:.3rem .6rem;border-bottom:1px solid #dde3ea;vertical-align:top}
code{font:13px ui-monospace,monospace}.fail{color:#b4471b}.muted{color:#5f6b7a}
</style></head><body>
<h1>CodeInstances store</h1>
<p class="muted">{{.Count}} instances, {{.Failing}} of them failing, in {{len .Groups}} groups. Codeinstances {{.Version}}.</p>
<h2>Failures by fingerprint</h2>
{{if .Groups}}<table><tr><th>Runs</th><th>Fingerprint</th><th>Failure</th><th>Last seen</th><th>Latest instance</th></tr>
{{range .Groups}}<tr><td>{{len .Runs}}</td><td><code>{{.Fingerprint.ID}}</code></td><td>{{.Fingerprint.Summary}}</td><td>{{if .Last}}{{.Last.UTC.Format "2006-01-02 15:04 UTC"}}{{end}}</td>
<td><a href="v1/instances/{{index .Runs (last .Runs)}}"><code>{{short (index .Runs (last .Runs))}}</code></a></td></tr>
{{end}}</table>{{else}}<p>No failing instances.</p>{{end}}
<h2>Latest instances</h2>
{{if .Entries}}<table><tr><th>Recorded</th><th>Instance</th><th>Command</th><th>Ended</th><th>Calls</th><th>Signed by</th></tr>
{{range .Entries}}<tr><td>{{.Created.UTC.Format "2006-01-02 15:04 UTC"}}</td><td><a href="v1/instances/{{.ID}}"><code>{{short .ID}}</code></a></td>
<td><code>{{join .Command}}</code></td><td{{if .Failed}} class="fail"{{end}}>{{.Ended}}</td><td>{{.Calls}}</td><td>{{range .SignedBy}}<code>{{.}}</code> {{end}}</td></tr>
{{end}}</table>{{else}}<p>The store is empty. Push an instance with <code>codeinstances push</code>.</p>{{end}}
</body></html>
`))

func (s *Server) index(w http.ResponseWriter) {
	entries := s.Store.List()
	g := s.Store.Groups()
	shown := entries
	if len(shown) > 100 {
		shown = shown[:100]
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	page.Execute(w, map[string]any{"Count": len(entries), "Failing": g.Failing, "Groups": g.Groups, "Entries": shown, "Version": codeinstances.Version})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	enc.Encode(v)
}

func problem(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
