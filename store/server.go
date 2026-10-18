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
	"net/http"
	"strings"

	"github.com/codeinstances/codeinstances"
	"github.com/codeinstances/codeinstances/instance"
)

// DefaultMaxBytes is the largest instance a server takes by default.
const DefaultMaxBytes = 256 << 20

// Server serves a store over HTTP:
//
//	GET  /                      a page listing the groups and the latest instances
//	POST /v1/instances          store the instance in the request body
//	GET  /v1/instances          list the stored instances as JSON (?failed=true for failing ones)
//	GET  /v1/instances/{id}     download an instance by id or unique prefix
//	GET  /v1/groups             the failing instances grouped by fingerprint, as JSON
type Server struct {
	Store *Store
	// Token, when set, must be sent as "Authorization: Bearer <token>".
	// It's needed to upload, and to read too unless OpenRead is set.
	Token    string
	OpenRead bool
	// MaxBytes limits an upload; zero means DefaultMaxBytes.
	MaxBytes int64
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	path := strings.TrimSuffix(r.URL.Path, "/")
	write := r.Method == http.MethodPost
	if !s.allowed(r, write) {
		w.Header().Set("WWW-Authenticate", `Bearer realm="codeinstances"`)
		problem(w, http.StatusUnauthorized, "a token is needed: send Authorization: Bearer <token>")
		return
	}
	switch {
	case path == "" && r.Method == http.MethodGet:
		s.index(w, r)
	case path == "/v1/instances" && r.Method == http.MethodPost:
		s.upload(w, r)
	case path == "/v1/instances" && r.Method == http.MethodGet:
		s.list(w, r)
	case strings.HasPrefix(path, "/v1/instances/") && r.Method == http.MethodGet:
		s.download(w, r, strings.TrimPrefix(path, "/v1/instances/"))
	case path == "/v1/groups" && r.Method == http.MethodGet:
		g, err := s.Store.Groups()
		if err != nil {
			problem(w, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, http.StatusOK, g)
	default:
		problem(w, http.StatusNotFound, "no such page")
	}
}

func (s *Server) allowed(r *http.Request, write bool) bool {
	if s.Token == "" || !write && s.OpenRead {
		return true
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) == 1
}

func (s *Server) upload(w http.ResponseWriter, r *http.Request) {
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
		problem(w, http.StatusBadRequest, err.Error())
		return
	}
	id, created, err := s.Store.Put(data)
	if err != nil {
		if errors.Is(err, instance.ErrModified) {
			problem(w, http.StatusUnprocessableEntity, err.Error())
			return
		}
		problem(w, http.StatusBadRequest, err.Error())
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, Pushed{ID: id, URL: baseURL(r) + "/v1/instances/" + id, Created: created})
}

func (s *Server) list(w http.ResponseWriter, r *http.Request) {
	entries, err := s.Store.List()
	if err != nil {
		problem(w, http.StatusInternalServerError, err.Error())
		return
	}
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

func (s *Server) download(w http.ResponseWriter, r *http.Request, ref string) {
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
		problem(w, http.StatusInternalServerError, err.Error())
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
{{if .Entries}}<table><tr><th>Recorded</th><th>Instance</th><th>Command</th><th>Ended</th><th>Calls</th></tr>
{{range .Entries}}<tr><td>{{.Created.UTC.Format "2006-01-02 15:04 UTC"}}</td><td><a href="v1/instances/{{.ID}}"><code>{{short .ID}}</code></a></td>
<td><code>{{join .Command}}</code></td><td{{if .Failed}} class="fail"{{end}}>{{.Ended}}</td><td>{{.Calls}}</td></tr>
{{end}}</table>{{else}}<p>The store is empty. Push an instance with <code>codeinstances push</code>.</p>{{end}}
</body></html>
`))

func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	entries, err := s.Store.List()
	if err != nil {
		problem(w, http.StatusInternalServerError, err.Error())
		return
	}
	g, err := s.Store.Groups()
	if err != nil {
		problem(w, http.StatusInternalServerError, err.Error())
		return
	}
	shown := entries
	if len(shown) > 100 {
		shown = shown[:100]
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
	page.Execute(w, map[string]any{"Count": len(entries), "Failing": g.Failing, "Groups": g.Groups, "Entries": shown, "Version": codeinstances.Version})
}

func baseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
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
