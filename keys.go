// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package codeinstances

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/codeinstances/codeinstances/instance"
)

// PublicKeyPrefix starts every line of a public key or trust file.
const PublicKeyPrefix = "codeinstances-ed25519"

// GenerateKey makes a new Ed25519 key pair for signing instances.
func GenerateKey() (ed25519.PrivateKey, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	return key, err
}

// EncodePrivateKey writes a private key as a PEM "PRIVATE KEY" block
// (PKCS #8), the form OpenSSL and most tools read.
func EncodePrivateKey(key ed25519.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// ParsePrivateKey reads what EncodePrivateKey wrote, or any PKCS #8 Ed25519
// key in PEM.
func ParsePrivateKey(data []byte) (ed25519.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PRIVATE KEY" {
		return nil, errors.New("not a PEM \"PRIVATE KEY\" file")
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := k.(ed25519.PrivateKey)
	if !ok {
		return nil, errors.New("not an Ed25519 key")
	}
	return key, nil
}

// ReadPrivateKey reads a private key file. Except on Windows, where the
// mode bits don't say who can read a file, it refuses a file that group or
// others can read or write, since anyone who can read it can sign as you.
func ReadPrivateKey(path string) (ed25519.PrivateKey, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s: other users can read or change this key (mode %#o); run chmod 600 %s", path, info.Mode().Perm(), path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	key, err := ParsePrivateKey(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return key, nil
}

// PublicKeyLine is a public key as one line of a trust file:
// "codeinstances-ed25519 <base64 key> <label>".
func PublicKeyLine(pub ed25519.PublicKey, label string) string {
	line := PublicKeyPrefix + " " + base64.StdEncoding.EncodeToString(pub)
	if label = strings.Join(strings.Fields(SafeText(label)), " "); label != "" {
		line += " " + label
	}
	return line + "\n"
}

// TrustedKey is one key from a trust file.
type TrustedKey struct {
	Key ed25519.PublicKey
	// Label is the rest of the key's line, a name for the key.
	Label string
}

// ID is the key's short id, as instance.KeyID.
func (k TrustedKey) ID() string { return instance.KeyID(k.Key) }

// ParseTrust reads a trust file: one public key line per key, as keygen
// writes them, with blank lines and lines starting with # ignored. A trust
// file is the .pub files of the keys you trust, put together.
func ParseTrust(data []byte) ([]TrustedKey, error) {
	var keys []TrustedKey
	sc := bufio.NewScanner(bytes.NewReader(data))
	n := 0
	for sc.Scan() {
		n++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 2 || f[0] != PublicKeyPrefix {
			return nil, fmt.Errorf("line %d: want %q, a base64 key and a label", n, PublicKeyPrefix)
		}
		k, err := base64.StdEncoding.DecodeString(f[1])
		if err != nil || len(k) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("line %d: not an Ed25519 public key", n)
		}
		keys = append(keys, TrustedKey{Key: ed25519.PublicKey(k), Label: SafeText(strings.Join(f[2:], " "))})
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, errors.New("no keys in the trust file")
	}
	return keys, nil
}

// ReadTrust reads trust files and puts their keys together.
func ReadTrust(paths ...string) ([]TrustedKey, error) {
	var all []TrustedKey
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		keys, err := ParseTrust(data)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		all = append(all, keys...)
	}
	return all, nil
}

// ErrNotTrusted is returned when an instance has no signature by a trusted
// key.
var ErrNotTrusted = errors.New("no signature by a trusted key")

// SignatureCheck is what a signature on an instance turned out to be.
type SignatureCheck struct {
	Signature instance.Signature
	// KeyID is the signing key's short id ("" for an algorithm this version
	// doesn't check).
	KeyID string
	// Checked is false for an algorithm this version doesn't check. An
	// Ed25519 signature that was read at all is valid: instance.Parse
	// refuses a file with a bad one.
	Checked bool
	// Trusted is the trusted key that made it, or nil.
	Trusted *TrustedKey
}

// CheckSignatures matches an instance's signatures, already checked by
// instance.Parse, against the trusted keys. It returns ErrNotTrusted when
// trusted is not empty and none of them signed. With no trusted keys it
// only lists the signatures.
func CheckSignatures(inst *instance.Instance, trusted []TrustedKey) ([]SignatureCheck, error) {
	var out []SignatureCheck
	ok := false
	for _, s := range inst.Signatures {
		c := SignatureCheck{Signature: s}
		if s.Alg == instance.AlgEd25519 {
			k := s.Key()
			c.KeyID, c.Checked = instance.KeyID(k), true
			for i := range trusted {
				if trusted[i].Key.Equal(k) {
					c.Trusted = &trusted[i]
					ok = true
					break
				}
			}
		}
		out = append(out, c)
	}
	if len(trusted) > 0 && !ok {
		return out, ErrNotTrusted
	}
	return out, nil
}
