// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package instance reads and writes instance files.
//
// An instance is one run of a program, kept as a file: the command, what it
// printed, how it exited, every HTTP exchange it made and every clock or
// random value it asked for through the hook package. The file is plain JSON
// in the open format described in docs/FORMAT.md, version "codeinstances/0".
//
// Every instance carries an id, the SHA-256 of its canonical form (see
// Canonical). Save sets it, Load and Verify check it, so an edited file is
// noticed.
package instance

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Format is the format string every instance file of this version carries.
const Format = "codeinstances/0"

// Kinds of call. Version 0 records HTTP exchanges only.
const KindHTTP = "http"

// Kinds of value, as written by the hook package.
const (
	ValueClock  = "clock"  // a time, RFC 3339 with nanoseconds
	ValueRandom = "random" // an unsigned 64-bit integer in decimal
	ValueBytes  = "bytes"  // random bytes, base64
)

// Instance is one recorded run.
type Instance struct {
	Format  string    `json:"format"`
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
	Tool    string    `json:"tool"`
	Command Command   `json:"command"`
	Result  Result    `json:"result"`
	Calls   []Call    `json:"calls"`
	Values  []Value   `json:"values"`
}

// Command is what was run.
type Command struct {
	Argv []string `json:"argv"`
	// Dir is the working directory at recording time, for the record only;
	// replay runs in its own working directory.
	Dir string `json:"dir,omitempty"`
	// Env holds the environment variables the recording was asked to keep
	// (codeinstances record -env NAME). Replay sets them again.
	Env map[string]string `json:"env,omitempty"`
}

// Result is how the run ended and what it printed.
type Result struct {
	ExitCode   int    `json:"exit_code"`
	Signal     string `json:"signal,omitempty"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	DurationMS int64  `json:"duration_ms"`
	Stdout     *Body  `json:"stdout,omitempty"`
	Stderr     *Body  `json:"stderr,omitempty"`
	// StdoutTruncated and StderrTruncated mean the output went past the
	// capture limit and only its start was kept.
	StdoutTruncated bool `json:"stdout_truncated,omitempty"`
	StderrTruncated bool `json:"stderr_truncated,omitempty"`
}

// Call is one exchange between the program and the outside world.
type Call struct {
	Seq  int    `json:"seq"`
	Kind string `json:"kind"`
	// AtMS is when the request arrived, in milliseconds since the run started.
	AtMS int64 `json:"at_ms"`
	// DurationMS is how long the exchange took, from the request's arrival
	// to the end of the response or the error. Recordings made before
	// version 0.2.0 don't have it.
	DurationMS *int64    `json:"duration_ms,omitempty"`
	Request    Request   `json:"request"`
	Response   *Response `json:"response,omitempty"`
	// Error is set when no response came back (the connection was refused,
	// say). The program then got a 502 from codeinstances, in recording and
	// in replay alike.
	Error string `json:"error,omitempty"`
}

// Request is an HTTP request as the program sent it.
type Request struct {
	Method string              `json:"method"`
	URL    string              `json:"url"`
	Header map[string][]string `json:"header,omitempty"`
	Body   *Body               `json:"body,omitempty"`
}

// Response is an HTTP response as the program received it.
type Response struct {
	Status int                 `json:"status"`
	Header map[string][]string `json:"header,omitempty"`
	Body   *Body               `json:"body,omitempty"`
}

// Value is one clock or random value the program asked for.
type Value struct {
	Seq   int    `json:"seq"`
	Kind  string `json:"kind"`
	Value string `json:"value"`
}

// Body holds bytes: as text when they are valid UTF-8, otherwise as base64.
// Exactly one field is set.
type Body struct {
	Text   string `json:"text,omitempty"`
	Base64 string `json:"base64,omitempty"`
}

// NewBody returns nil for no bytes, so empty bodies stay out of the file.
func NewBody(b []byte) *Body {
	if len(b) == 0 {
		return nil
	}
	if utf8.Valid(b) {
		return &Body{Text: string(b)}
	}
	return &Body{Base64: base64.StdEncoding.EncodeToString(b)}
}

// Bytes returns the body's bytes. A nil body has none.
func (b *Body) Bytes() ([]byte, error) {
	if b == nil {
		return nil, nil
	}
	if b.Text != "" && b.Base64 != "" {
		return nil, errors.New("body has both text and base64")
	}
	if b.Base64 != "" {
		return base64.StdEncoding.DecodeString(b.Base64)
	}
	return []byte(b.Text), nil
}

// MustBytes is Bytes for bodies already checked by Validate.
func (b *Body) MustBytes() []byte {
	out, err := b.Bytes()
	if err != nil {
		panic(err)
	}
	return out
}

// Canonical returns the bytes the id is the hash of, by the rules in
// docs/FORMAT.md: the file's JSON with the top-level "id" member removed,
// member names sorted by their UTF-8 bytes, no whitespace between tokens,
// numbers exactly as written, and strings escaped only where they must be.
// Files that aren't valid UTF-8, hold unpaired surrogate escapes or repeat a
// member name in one object are refused.
func Canonical(data []byte) ([]byte, error) {
	v, err := parseJSON(data)
	if err != nil {
		return nil, err
	}
	if v.kind != jobj {
		return nil, errors.New("not a JSON object")
	}
	if _, ok := v.items["id"]; ok {
		delete(v.items, "id")
		keys := v.keys[:0:0]
		for _, k := range v.keys {
			if k != "id" {
				keys = append(keys, k)
			}
		}
		v.keys = keys
	}
	var out bytes.Buffer
	writeCanonical(&out, v)
	return out.Bytes(), nil
}

// ComputeID returns "sha256:" and the hex SHA-256 of Canonical(data).
func ComputeID(data []byte) (string, error) {
	canon, err := Canonical(data)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// Marshal sets the format and the id and returns the file's bytes.
func Marshal(inst *Instance) ([]byte, error) {
	inst.Format = Format
	if inst.Calls == nil {
		inst.Calls = []Call{}
	}
	if inst.Values == nil {
		inst.Values = []Value{}
	}
	if err := inst.Validate(); err != nil {
		return nil, err
	}
	inst.ID = ""
	plain, err := encode(inst)
	if err != nil {
		return nil, err
	}
	id, err := ComputeID(plain)
	if err != nil {
		return nil, err
	}
	inst.ID = id
	return encode(inst)
}

func encode(inst *Instance) ([]byte, error) {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(inst); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// Save writes the instance to path. It writes a temporary file next to it,
// flushes it to disk and renames it into place, so path holds either the
// whole new file or whatever it held before, never part of one.
func Save(path string, inst *Instance) error {
	data, err := Marshal(inst)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(name)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		return err
	}
	ok = true
	return nil
}

// Load reads an instance file, checks its format, its id and its contents.
func Load(path string) (*Instance, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	inst, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return inst, nil
}

// Parse is Load for bytes already read.
func Parse(data []byte) (*Instance, error) {
	tree, err := parseJSON(data)
	if err != nil {
		return nil, fmt.Errorf("not an instance file: %w", err)
	}
	if tree.kind != jobj {
		return nil, errors.New("not an instance file: not a JSON object")
	}
	format, ok := tree.items["format"]
	if !ok || format.kind != jstr || format.s == "" {
		return nil, errors.New("not an instance file: no format")
	}
	if format.s != Format {
		return nil, fmt.Errorf("unsupported format %q (this version reads %q)", format.s, Format)
	}
	var inst Instance
	if err := json.Unmarshal(data, &inst); err != nil {
		return nil, fmt.Errorf("damaged instance file: %w", err)
	}
	if err := VerifyBytes(data); err != nil {
		return nil, err
	}
	// What Go read into the struct must be exactly what the file says, so a
	// member spelled in another case can't be read one way here and another
	// way elsewhere.
	read, err := encode(&inst)
	if err != nil {
		return nil, err
	}
	readTree, err := parseJSON(read)
	if err != nil {
		return nil, err
	}
	if err := covers(tree, readTree, "instance"); err != nil {
		return nil, fmt.Errorf("damaged instance file: %w", err)
	}
	if err := inst.Validate(); err != nil {
		return nil, err
	}
	return &inst, nil
}

// ErrModified is returned when an instance file's id doesn't match its
// contents.
var ErrModified = errors.New("the file was changed after it was recorded: its id doesn't match its contents")

// VerifyBytes checks that the id in the file is the hash of its contents.
func VerifyBytes(data []byte) error {
	tree, err := parseJSON(data)
	if err != nil {
		return err
	}
	id, ok := tree.items["id"]
	if tree.kind != jobj || !ok || id.kind != jstr || id.s == "" {
		return errors.New("the file has no id")
	}
	want, err := ComputeID(data)
	if err != nil {
		return err
	}
	if id.s != want {
		return ErrModified
	}
	return nil
}

func (b *Body) check() error {
	if b != nil && b.Text == "" && b.Base64 == "" {
		return errors.New("an empty body is left out, not written as {}")
	}
	_, err := b.Bytes()
	return err
}

// Validate checks the instance's own consistency.
func (inst *Instance) Validate() error {
	if len(inst.Command.Argv) == 0 || inst.Command.Argv[0] == "" {
		return errors.New("instance has no command")
	}
	for _, b := range []*Body{inst.Result.Stdout, inst.Result.Stderr} {
		if err := b.check(); err != nil {
			return fmt.Errorf("result: %w", err)
		}
	}
	last := 0
	for i, c := range inst.Calls {
		if c.Seq <= last {
			return fmt.Errorf("call %d: seq %d isn't above %d", i, c.Seq, last)
		}
		last = c.Seq
		if c.Kind != KindHTTP {
			return fmt.Errorf("call %d: unknown kind %q", c.Seq, c.Kind)
		}
		if c.DurationMS != nil && *c.DurationMS < 0 {
			return fmt.Errorf("call %d: duration %d ms", c.Seq, *c.DurationMS)
		}
		if c.Request.Method == "" || c.Request.URL == "" {
			return fmt.Errorf("call %d: request has no method or URL", c.Seq)
		}
		if err := c.Request.Body.check(); err != nil {
			return fmt.Errorf("call %d request: %w", c.Seq, err)
		}
		if (c.Response == nil) == (c.Error == "") {
			return fmt.Errorf("call %d: needs exactly one of response and error", c.Seq)
		}
		if c.Response != nil {
			if c.Response.Status < 100 || c.Response.Status > 999 {
				return fmt.Errorf("call %d: status %d", c.Seq, c.Response.Status)
			}
			if err := c.Response.Body.check(); err != nil {
				return fmt.Errorf("call %d response: %w", c.Seq, err)
			}
		}
	}
	last = 0
	for i, v := range inst.Values {
		if v.Seq <= last {
			return fmt.Errorf("value %d: seq %d isn't above %d", i, v.Seq, last)
		}
		last = v.Seq
		var err error
		switch v.Kind {
		case ValueClock:
			_, err = time.Parse(time.RFC3339Nano, v.Value)
		case ValueRandom:
			_, err = strconv.ParseUint(v.Value, 10, 64)
		case ValueBytes:
			_, err = base64.StdEncoding.DecodeString(v.Value)
		default:
			return fmt.Errorf("value %d: unknown kind %q", v.Seq, v.Kind)
		}
		if err != nil {
			return fmt.Errorf("value %d: not a %s value: %v", v.Seq, v.Kind, err)
		}
	}
	return nil
}

// Summary is a one-line description of the instance.
func (inst *Instance) Summary() string {
	how := fmt.Sprintf("exit %d", inst.Result.ExitCode)
	if inst.Result.Signal != "" {
		how = "killed by " + inst.Result.Signal
	}
	if inst.Result.TimedOut {
		how = "timed out"
	}
	return fmt.Sprintf("%s: %s, %s, %d calls, %d values, %d ms", short(inst.ID), strings.Join(inst.Command.Argv, " "), how, len(inst.Calls), len(inst.Values), inst.Result.DurationMS)
}

func short(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// ShortID is the first 12 hex digits of an id, enough to tell files apart.
func ShortID(id string) string { return short(id) }
