// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package instance

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, k, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

var signTime = time.Date(2026, 10, 18, 9, 30, 15, 123, time.FixedZone("IDT", 3*3600))

// reid gives edited bytes the id that matches them, as anyone can.
func reid(t *testing.T, data []byte) []byte {
	t.Helper()
	id, err := ComputeID(data)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Replace(data, []byte(sample0ID(data)), []byte(id), 1)
}

func TestSignRoundTrip(t *testing.T) {
	plain, err := Marshal(sample())
	if err != nil {
		t.Fatal(err)
	}
	unsignedID := sample0ID(plain)
	inst := sample()
	key := newKey(t)
	if err := inst.Sign(key, "ci", signTime); err != nil {
		t.Fatal(err)
	}
	data, err := Marshal(inst)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID == unsignedID {
		t.Fatal("the id should cover the signatures")
	}
	content, _ := ContentID(data)
	s := got.Signatures[0]
	if content != unsignedID || s.Content != unsignedID {
		t.Fatalf("the content id should be the unsigned file's id: %s %s %s", content, s.Content, unsignedID)
	}
	if s.SignedAt != "2026-10-18T06:30:15Z" || s.Signer != "ci" || s.Alg != AlgEd25519 {
		t.Fatalf("%+v", s)
	}
	if !s.Key().Equal(key.Public()) || KeyID(s.Key()) == "" || len(KeyID(s.Key())) != 16 {
		t.Fatal("key")
	}

	// A second key adds a signature; the same key again replaces its own.
	other := newKey(t)
	got.Sign(other, "", signTime)
	got.Sign(key, "ci-again", signTime)
	data2, err := Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	two, err := Parse(data2)
	if err != nil {
		t.Fatal(err)
	}
	if len(two.Signatures) != 2 || two.Signatures[1].Signer != "ci-again" {
		t.Fatalf("%+v", two.Signatures)
	}
	if c, _ := ContentID(data2); c != unsignedID {
		t.Fatal("the content id moved")
	}
}

func signedSample(t *testing.T) ([]byte, ed25519.PrivateKey) {
	t.Helper()
	inst := sample()
	key := newKey(t)
	if err := inst.Sign(key, "ci", signTime); err != nil {
		t.Fatal(err)
	}
	data, err := Marshal(inst)
	if err != nil {
		t.Fatal(err)
	}
	return data, key
}

func TestSignatureCatchesEditsWithANewID(t *testing.T) {
	data, _ := signedSample(t)
	for name, edit := range map[string][2]string{
		"output":    {`hello <world>`, `hello <World>`},
		"exit code": {`"exit_code": 3`, `"exit_code": 0`},
		"signer":    {`"signer": "ci"`, `"signer": "cj"`},
		"time":      {`"signed_at": "2026-10-18T06:30:15Z"`, `"signed_at": "2026-10-17T06:30:15Z"`},
	} {
		changed := bytes.Replace(data, []byte(edit[0]), []byte(edit[1]), 1)
		if bytes.Equal(changed, data) {
			t.Fatalf("%s: setup", name)
		}
		if _, err := Parse(changed); !errors.Is(err, ErrModified) {
			t.Errorf("%s, old id: %v", name, err)
		}
		if _, err := Parse(reid(t, changed)); !errors.Is(err, ErrBadSignature) {
			t.Errorf("%s, new id: %v", name, err)
		}
	}

	// A signature moved onto another run vouches for other content.
	var doc map[string]json.RawMessage
	json.Unmarshal(data, &doc)
	other := sample()
	other.Result.ExitCode = 0
	odata, _ := Marshal(other)
	var odoc map[string]json.RawMessage
	json.Unmarshal(odata, &odoc)
	odoc["signatures"] = doc["signatures"]
	moved, _ := json.Marshal(odoc)
	if _, err := Parse(reid(t, moved)); !errors.Is(err, ErrBadSignature) || !strings.Contains(err.Error(), "key ") {
		t.Fatalf("moved signature: %v", err)
	}

	// Taking the signature off leaves a plain, unsigned file: removal can't
	// be stopped, only noticed, by requiring a trusted signature.
	delete(doc, "signatures")
	stripped, _ := json.Marshal(doc)
	inst, err := Parse(reid(t, stripped))
	if err != nil || len(inst.Signatures) != 0 {
		t.Fatalf("stripped: %v", err)
	}
}

func TestSignatureForms(t *testing.T) {
	data, _ := signedSample(t)
	good, _ := Parse(data)
	for name, edit := range map[string]func(s *Signature){
		"no alg":        func(s *Signature) { s.Alg = "" },
		"bad content":   func(s *Signature) { s.Content = "sha256:abc" },
		"local time":    func(s *Signature) { s.SignedAt = "2026-10-18T09:30:15+03:00" },
		"fractional":    func(s *Signature) { s.SignedAt = "2026-10-18T06:30:15.5Z" },
		"signer break":  func(s *Signature) { s.Signer = "ci\ncontent x" },
		"signer escape": func(s *Signature) { s.Signer = "ci\x1b[2J" },
		"long signer":   func(s *Signature) { s.Signer = strings.Repeat("a", 101) },
		"short key":     func(s *Signature) { s.PublicKey = "AAAA" },
		"short sig":     func(s *Signature) { s.Value = "AAAA" },
	} {
		inst := *good
		inst.Signatures = append([]Signature(nil), good.Signatures...)
		edit(&inst.Signatures[0])
		if err := inst.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	twice := *good
	twice.Signatures = append(append([]Signature(nil), good.Signatures...), good.Signatures[0])
	if err := twice.Validate(); err == nil || !strings.Contains(err.Error(), "twice") {
		t.Fatalf("same key twice: %v", err)
	}

	// A signature in an algorithm from a later version is kept, unchecked.
	later := *good
	later.Signatures = []Signature{{Alg: "mldsa65", PublicKey: "x", Content: good.Signatures[0].Content, SignedAt: "2027-01-01T00:00:00Z", Value: "y"}}
	ldata, err := Marshal(&later)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := Parse(ldata); err != nil || len(got.Signatures) != 1 {
		t.Fatalf("later algorithm: %v", err)
	}

	inst := sample()
	if err := inst.Sign(newKey(t), "ci\nx", signTime); err == nil {
		t.Fatal("signer with a line break signed")
	}
	if err := inst.Sign(ed25519.PrivateKey{1, 2}, "", signTime); err == nil {
		t.Fatal("short key signed")
	}
}

func TestMessageIsFixed(t *testing.T) {
	s := Signature{Content: "sha256:" + strings.Repeat("ab", 32), PublicKey: "KEY", SignedAt: "2026-10-18T06:30:15Z", Signer: "ci"}
	want := "codeinstances/0 signature\ncontent sha256:" + strings.Repeat("ab", 32) + "\nkey KEY\nsigned_at 2026-10-18T06:30:15Z\nsigner ci\n"
	if string(s.Message()) != want {
		t.Fatalf("%q", s.Message())
	}
}

// edit applies change to the signed sample's JSON tree and writes a
// matching id, as anyone could.
func editSigned(t *testing.T, data []byte, change func(doc map[string]any)) []byte {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	change(doc)
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return reid(t, out)
}

func TestSignatureBytesCantBeRespelled(t *testing.T) {
	data, _ := signedSample(t)
	sig := func(doc map[string]any) map[string]any {
		return doc["signatures"].([]any)[0].(map[string]any)
	}
	if _, err := Parse(editSigned(t, data, func(map[string]any) {})); err != nil {
		t.Fatalf("setup: %v", err)
	}
	for name, change := range map[string]func(doc map[string]any){
		"padding bits": func(doc map[string]any) {
			v := sig(doc)["signature"].(string)
			// The last character before "==" carries 4 unused bits.
			b := []byte(v)
			i := len(b) - 3
			const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
			p := strings.IndexByte(alphabet, b[i])
			b[i] = alphabet[p^1]
			sig(doc)["signature"] = string(b)
		},
		"line break in signature": func(doc map[string]any) {
			v := sig(doc)["signature"].(string)
			sig(doc)["signature"] = v[:10] + "\n" + v[10:]
		},
		"line break in key": func(doc map[string]any) {
			v := sig(doc)["public_key"].(string)
			sig(doc)["public_key"] = v[:10] + "\r\n" + v[10:]
		},
		"extra member": func(doc map[string]any) { sig(doc)["note"] = "anything at all" },
		"too many": func(doc map[string]any) {
			list := doc["signatures"].([]any)
			for i := 0; i < 16; i++ {
				list = append(list, map[string]any{"alg": "mldsa65", "public_key": fmt.Sprint(i), "content": sig(doc)["content"], "signed_at": "2027-01-01T00:00:00Z", "signature": "x"})
			}
			doc["signatures"] = list
		},
		"huge unknown entry": func(doc map[string]any) {
			doc["signatures"] = append(doc["signatures"].([]any), map[string]any{"alg": "mldsa65", "public_key": strings.Repeat("A", 20000), "content": sig(doc)["content"], "signed_at": "2027-01-01T00:00:00Z", "signature": "x"})
		},
	} {
		if _, err := Parse(editSigned(t, data, change)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// The signed example: docs/example.instance signed by a key made from a
// fixed seed, so other implementations can check theirs against it.
func exampleKey() ed25519.PrivateKey {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	return ed25519.NewKeyFromSeed(seed)
}

func TestSignedExample(t *testing.T) {
	path := filepath.Join("..", "docs", "example-signed.instance")
	inst, err := Load(filepath.Join("..", "docs", "example.instance"))
	if err != nil {
		t.Fatal(err)
	}
	if err := inst.Sign(exampleKey(), "example", time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	want, err := Marshal(inst)
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("CODEINSTANCES_WRITE_EXAMPLE") == "1" {
		os.WriteFile(path, want, 0o644)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatal("docs/example-signed.instance isn't the signed example (set CODEINSTANCES_WRITE_EXAMPLE=1 to write it)")
	}
	signed, err := Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	plain, _ := os.ReadFile(filepath.Join("..", "docs", "example.instance"))
	if c, _ := ContentID(got); c != sample0ID(plain) || signed.Signatures[0].Content != c {
		t.Fatal("the signed example's content id isn't the example's id")
	}
}
