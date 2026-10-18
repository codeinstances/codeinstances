// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package codeinstances

import (
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/codeinstances/codeinstances/instance"
)

func TestKeyFiles(t *testing.T) {
	key, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	pemData, err := EncodePrivateKey(key)
	if err != nil || !strings.HasPrefix(string(pemData), "-----BEGIN PRIVATE KEY-----") {
		t.Fatalf("%v %s", err, pemData)
	}
	back, err := ParsePrivateKey(pemData)
	if err != nil || !back.Equal(key) {
		t.Fatalf("round trip: %v", err)
	}
	for _, bad := range []string{"", "not pem", "-----BEGIN PUBLIC KEY-----\nAAAA\n-----END PUBLIC KEY-----\n"} {
		if _, err := ParsePrivateKey([]byte(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "k.key")
	os.WriteFile(path, pemData, 0o600)
	if _, err := ReadPrivateKey(path); err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		os.Chmod(path, 0o644)
		if _, err := ReadPrivateKey(path); err == nil || !strings.Contains(err.Error(), "chmod 600") {
			t.Fatalf("a key others can read: %v", err)
		}
	}

	pub := key.Public().(ed25519.PublicKey)
	line := PublicKeyLine(pub, "ci  runner\x1b[2J")
	if !strings.HasPrefix(line, PublicKeyPrefix+" ") || strings.Contains(line, "\x1b") || !strings.HasSuffix(line, " ci runner\n") {
		t.Fatalf("%q", line)
	}
	other, _ := GenerateKey()
	trust := "# keys we trust\n\n" + line + PublicKeyLine(other.Public().(ed25519.PublicKey), "")
	keys, err := ParseTrust([]byte(trust))
	if err != nil || len(keys) != 2 || keys[0].Label != "ci runner" || !keys[0].Key.Equal(pub) || keys[1].ID() != instance.KeyID(other.Public().(ed25519.PublicKey)) {
		t.Fatalf("%v %+v", err, keys)
	}
	for _, bad := range []string{"", "# only a comment\n", "ssh-ed25519 AAAA x\n", PublicKeyPrefix + " AAAA\n", PublicKeyPrefix + "\n"} {
		if _, err := ParseTrust([]byte(bad)); err == nil {
			t.Errorf("trust %q parsed", bad)
		}
	}
	tp := filepath.Join(dir, "trust")
	os.WriteFile(tp, []byte(trust), 0o644)
	if k, err := ReadTrust(tp, tp); err != nil || len(k) != 4 {
		t.Fatal(err)
	}
	if _, err := ReadTrust(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing trust file")
	}
}

func TestCheckSignatures(t *testing.T) {
	key, _ := GenerateKey()
	other, _ := GenerateKey()
	inst := &instance.Instance{Tool: "t", Command: instance.Command{Argv: []string{"x"}}}
	if c, err := CheckSignatures(inst, nil); err != nil || len(c) != 0 {
		t.Fatal(err)
	}
	trusted := []TrustedKey{{Key: key.Public().(ed25519.PublicKey), Label: "ci"}}
	if _, err := CheckSignatures(inst, trusted); !errors.Is(err, ErrNotTrusted) {
		t.Fatalf("unsigned: %v", err)
	}
	inst.Sign(other, "someone", time.Now())
	if c, err := CheckSignatures(inst, trusted); !errors.Is(err, ErrNotTrusted) || len(c) != 1 || c[0].Trusted != nil || !c[0].Checked {
		t.Fatalf("signed by another key: %v", err)
	}
	inst.Sign(key, "ci", time.Now())
	c, err := CheckSignatures(inst, trusted)
	if err != nil || len(c) != 2 || c[1].Trusted == nil || c[1].Trusted.Label != "ci" || c[1].KeyID != instance.KeyID(key.Public().(ed25519.PublicKey)) {
		t.Fatalf("%v %+v", err, c)
	}
	// A later algorithm is listed but never counts as trusted.
	inst.Signatures = []instance.Signature{{Alg: "mldsa65"}}
	if c, err := CheckSignatures(inst, trusted); !errors.Is(err, ErrNotTrusted) || c[0].Checked {
		t.Fatal(err)
	}
}
