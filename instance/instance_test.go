// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package instance

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func sample() *Instance {
	return &Instance{
		Created: time.Date(2026, 10, 18, 7, 0, 0, 0, time.UTC),
		Tool:    "codeinstances test",
		Command: Command{Argv: []string{"prog", "-x"}, Env: map[string]string{"B": "2", "A": "1"}},
		Result:  Result{ExitCode: 3, DurationMS: 12, Stdout: NewBody([]byte("hello <world> & \"you\"\n")), Stderr: NewBody([]byte{0xff, 0x00, 0x01})},
		Calls: []Call{
			{Seq: 1, Kind: KindHTTP, AtMS: 1, Request: Request{Method: "GET", URL: "https://a.test/x?y=1", Header: map[string][]string{"Accept": {"*/*"}}},
				Response: &Response{Status: 200, Body: NewBody([]byte(`{"ok":true}`))}},
			{Seq: 2, Kind: KindHTTP, AtMS: 5, Request: Request{Method: "POST", URL: "http://b.test/", Body: NewBody([]byte("data"))}, Error: "connection refused"},
		},
		Values: []Value{{Seq: 1, Kind: ValueClock, Value: "2026-10-18T07:00:00Z"}, {Seq: 2, Kind: ValueRandom, Value: "42"}},
	}
}

func TestBodyTextAndBinary(t *testing.T) {
	if NewBody(nil) != nil || NewBody([]byte{}) != nil {
		t.Fatal("empty bodies should be nil")
	}
	for _, in := range [][]byte{[]byte("plain text ✓"), {0xff, 0xfe, 0, 1, 2}, bytes.Repeat([]byte{0x80}, 1000)} {
		b := NewBody(in)
		out, err := b.Bytes()
		if err != nil || !bytes.Equal(in, out) {
			t.Fatalf("round trip of %x: %x, %v", in, out, err)
		}
	}
	if NewBody([]byte{0xff}).Base64 == "" {
		t.Fatal("invalid UTF-8 should be base64")
	}
	if NewBody([]byte("ok")).Text != "ok" {
		t.Fatal("UTF-8 should be text")
	}
	if _, err := (&Body{Text: "a", Base64: "YQ=="}).Bytes(); err == nil {
		t.Fatal("both fields set should fail")
	}
	if _, err := (&Body{Base64: "!!"}).Bytes(); err == nil {
		t.Fatal("bad base64 should fail")
	}
}

func TestSaveLoadRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "a.instance")
	in := sample()
	if err := Save(path, in); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(in.ID, "sha256:") || len(in.ID) != 71 {
		t.Fatalf("id %q", in.ID)
	}
	out, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if out.ID != in.ID || out.Format != Format || out.Command.Env["A"] != "1" || out.Result.ExitCode != 3 {
		t.Fatalf("loaded %+v", out)
	}
	if !bytes.Equal(out.Result.Stderr.MustBytes(), []byte{0xff, 0x00, 0x01}) {
		t.Fatal("binary stderr lost")
	}
	if len(out.Calls) != 2 || out.Calls[1].Error != "connection refused" || string(out.Calls[0].Response.Body.MustBytes()) != `{"ok":true}` {
		t.Fatalf("calls %+v", out.Calls)
	}
	data, _ := os.ReadFile(path)
	if bytes.Contains(data, []byte("\\"+"u003c")) {
		t.Fatal("HTML characters should not be escaped")
	}
}

func TestIDIsStableAndIgnoresLayout(t *testing.T) {
	a, _ := Marshal(sample())
	b, _ := Marshal(sample())
	if !bytes.Equal(a, b) {
		t.Fatal("same instance, different bytes")
	}
	// Whitespace and key order don't change the id.
	var buf bytes.Buffer
	json.Compact(&buf, a)
	compact := buf.Bytes()
	ida, _ := ComputeID(a)
	idc, _ := ComputeID(compact)
	if ida != idc {
		t.Fatal("layout changed the id")
	}
	reordered := []byte(`{"z":1,"a":2}`)
	same := []byte(`{ "a" : 2, "z" : 1 }`)
	x, _ := ComputeID(reordered)
	y, _ := ComputeID(same)
	if x != y {
		t.Fatal("key order changed the id")
	}
	// Numbers are kept as written.
	big1, _ := Canonical([]byte(`{"n":12345678901234567890}`))
	if string(big1) != `{"n":12345678901234567890}` {
		t.Fatalf("canonical %s", big1)
	}
}

func TestVerifyCatchesEdits(t *testing.T) {
	data, err := Marshal(sample())
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyBytes(data); err != nil {
		t.Fatal(err)
	}
	edits := []struct{ from, to string }{
		{`"exit_code": 3`, `"exit_code": 0`},
		{`"value": "42"`, `"value": "43"`},
		{`"status": 200`, `"status": 201`},
		{`connection refused`, `connection reset`},
	}
	for _, e := range edits {
		changed := bytes.Replace(data, []byte(e.from), []byte(e.to), 1)
		if bytes.Equal(changed, data) {
			t.Fatalf("edit %q not applied", e.from)
		}
		if err := VerifyBytes(changed); !errors.Is(err, ErrModified) {
			t.Fatalf("edit %q: got %v", e.from, err)
		}
		if _, err := Parse(changed); !errors.Is(err, ErrModified) {
			t.Fatalf("parse after edit %q: got %v", e.from, err)
		}
	}
	// An added member changes the id too.
	added := bytes.Replace(data, []byte(`"tool":`), []byte(`"extra": 1, "tool":`), 1)
	if err := VerifyBytes(added); !errors.Is(err, ErrModified) {
		t.Fatalf("added member: %v", err)
	}
}

func TestParseRejectsBadFiles(t *testing.T) {
	good, _ := Marshal(sample())
	cases := map[string][]byte{
		"empty":       {},
		"not json":    []byte("hello"),
		"no format":   []byte(`{"id":"x"}`),
		"future":      []byte(`{"format":"codeinstances/9","id":"x"}`),
		"truncated":   good[:len(good)/2],
		"trailing":    append(append([]byte{}, good...), []byte(`{}`)...),
		"no id":       bytes.Replace(good, []byte(`"id": "sha256:`), []byte(`"idx": "sha256:`), 1),
		"wrong types": []byte(`{"format":"codeinstances/0","id":"x","calls":"no"}`),
	}
	for name, data := range cases {
		if _, err := Parse(data); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
}

func TestValidate(t *testing.T) {
	bad := []func(*Instance){
		func(i *Instance) { i.Command.Argv = nil },
		func(i *Instance) { i.Calls[1].Seq = 1 },
		func(i *Instance) { i.Calls[0].Kind = "grpc" },
		func(i *Instance) { i.Calls[0].Request.URL = "" },
		func(i *Instance) { i.Calls[0].Error = "both" },
		func(i *Instance) { i.Calls[1].Error = "" },
		func(i *Instance) { i.Calls[0].Response.Status = 42 },
		func(i *Instance) { i.Values[1].Seq = 1 },
		func(i *Instance) { i.Values[0].Kind = "weather" },
		func(i *Instance) { i.Result.Stdout = &Body{Base64: "%%%"} },
	}
	for n, f := range bad {
		in := sample()
		f(in)
		if err := in.Validate(); err == nil {
			t.Errorf("case %d: valid", n)
		}
		if _, err := Marshal(in); err == nil {
			t.Errorf("case %d: marshalled", n)
		}
	}
	if err := sample().Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSaveIsAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.instance")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	bad := sample()
	bad.Command.Argv = nil
	if err := Save(path, bad); err == nil {
		t.Fatal("saved an invalid instance")
	}
	if b, _ := os.ReadFile(path); string(b) != "old" {
		t.Fatal("failed save changed the file")
	}
	if err := Save(path, sample()); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("left behind: %v", entries)
	}
	if err := Save(filepath.Join(dir, "missing", "a.instance"), sample()); err == nil {
		t.Fatal("saved into a missing directory")
	}
}

func TestSummary(t *testing.T) {
	in := sample()
	Marshal(in)
	s := in.Summary()
	if !strings.Contains(s, "prog -x") || !strings.Contains(s, "exit 3") || !strings.Contains(s, "2 calls") {
		t.Fatal(s)
	}
	in.Result.Signal = "killed"
	if !strings.Contains(in.Summary(), "killed by killed") {
		t.Fatal(in.Summary())
	}
	in.Result.TimedOut = true
	if !strings.Contains(in.Summary(), "timed out") {
		t.Fatal(in.Summary())
	}
}

// The example in docs/FORMAT.md must stay a valid, intact instance.
func TestDocumentedExample(t *testing.T) {
	inst, err := Load(filepath.Join("..", "docs", "example.instance"))
	if err != nil {
		t.Fatal(err)
	}
	if len(inst.Calls) != 1 || inst.Calls[0].Response.Status != 200 {
		t.Fatalf("%+v", inst.Calls)
	}
}
