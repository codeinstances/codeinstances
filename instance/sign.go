// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

package instance

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"
)

// AlgEd25519 is the one signature algorithm this version makes and checks.
const AlgEd25519 = "ed25519"

// Signature says that the holder of a key vouched for an instance's content.
//
// It signs the instance's content id: the id the file would have without
// its signatures (see ContentID). So any number of signatures can be added
// to one run, each by its own key, and none of them covers the others. The
// file's own id still covers everything, signatures included, so adding or
// removing one gives the file a new id while its content id stays the same.
type Signature struct {
	Alg string `json:"alg"`
	// PublicKey is the signing key, base64 (standard, padded).
	PublicKey string `json:"public_key"`
	// Content is the content id that was signed.
	Content string `json:"content"`
	// Signer is a name the signer gave, such as "ci" or "agent-runner-3".
	// Like SignedAt, it is the signer's claim; the key is what's checked.
	Signer string `json:"signer,omitempty"`
	// SignedAt is when the signature was made, RFC 3339 in UTC, by the
	// signer's clock.
	SignedAt string `json:"signed_at"`
	// Value is the Ed25519 signature of Message, base64.
	Value string `json:"signature"`
}

// ErrBadSignature is returned for a file whose signature doesn't check out:
// it was made for other content, or not by the key it names.
var ErrBadSignature = errors.New("a signature doesn't check out: the run was changed after it was signed, or the signature wasn't made by its key")

// Message is the exact bytes a signature signs. Fields can't hold line
// breaks, so the message reads only one way.
func (s *Signature) Message() []byte {
	return []byte(fmt.Sprintf("codeinstances/0 signature\ncontent %s\nkey %s\nsigned_at %s\nsigner %s\n", s.Content, s.PublicKey, s.SignedAt, s.Signer))
}

// Key returns the signature's public key, or nil when it isn't a valid
// Ed25519 key.
func (s *Signature) Key() ed25519.PublicKey {
	k, err := base64.StdEncoding.DecodeString(s.PublicKey)
	if err != nil || len(k) != ed25519.PublicKeySize {
		return nil
	}
	return ed25519.PublicKey(k)
}

// KeyID is a short name for a public key: the first 16 hex digits of its
// SHA-256.
func KeyID(k ed25519.PublicKey) string {
	sum := sha256.Sum256(k)
	return hex.EncodeToString(sum[:8])
}

// ContentID is the id the file would have without its signatures: the
// SHA-256 of the canonical form with both "id" and "signatures" removed.
func ContentID(data []byte) (string, error) {
	canon, err := canonicalWithout(data, "id", "signatures")
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canon)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

const maxSigner = 100

func (s *Signature) check() error {
	if s.Alg == "" || !httpToken.MatchString(s.Alg) {
		return errors.New("needs an alg")
	}
	if !parentID.MatchString(s.Content) {
		return errors.New("content isn't a sha256 id")
	}
	if t, err := time.Parse(time.RFC3339, s.SignedAt); err != nil || t.UTC().Format(time.RFC3339) != s.SignedAt {
		return errors.New("signed_at isn't an RFC 3339 time in UTC, to the second")
	}
	if hasControl(s.Signer) || !utf8.ValidString(s.Signer) || len(s.Signer) > maxSigner {
		return fmt.Errorf("the signer holds control characters or is over %d bytes", maxSigner)
	}
	if s.Alg != AlgEd25519 {
		// Another algorithm, from a later version: kept, but not checked.
		return nil
	}
	if s.Key() == nil {
		return errors.New("public_key isn't an Ed25519 key in base64")
	}
	v, err := base64.StdEncoding.DecodeString(s.Value)
	if err != nil || len(v) != ed25519.SignatureSize {
		return errors.New("signature isn't an Ed25519 signature in base64")
	}
	return nil
}

func (inst *Instance) checkSignatures() error {
	seen := map[string]bool{}
	for i := range inst.Signatures {
		s := &inst.Signatures[i]
		if err := s.check(); err != nil {
			return fmt.Errorf("signature %d: %w", i+1, err)
		}
		if seen[s.Alg+" "+s.PublicKey] {
			return fmt.Errorf("signature %d: the key signed twice", i+1)
		}
		seen[s.Alg+" "+s.PublicKey] = true
	}
	return nil
}

// verifySignatures checks every Ed25519 signature against the file's bytes.
func verifySignatures(inst *Instance, data []byte) error {
	if len(inst.Signatures) == 0 {
		return nil
	}
	content, err := ContentID(data)
	if err != nil {
		return err
	}
	for i := range inst.Signatures {
		s := &inst.Signatures[i]
		if s.Alg != AlgEd25519 {
			continue
		}
		v, _ := base64.StdEncoding.DecodeString(s.Value)
		if s.Content != content || !ed25519.Verify(s.Key(), s.Message(), v) {
			return fmt.Errorf("signature %d (key %s): %w", i+1, KeyID(s.Key()), ErrBadSignature)
		}
	}
	return nil
}

// Sign adds a signature by key to the instance, replacing any earlier one
// by the same key. Save or Marshal it afterwards; the file gets a new id.
func (inst *Instance) Sign(key ed25519.PrivateKey, signer string, at time.Time) error {
	if len(key) != ed25519.PrivateKeySize {
		return errors.New("not an Ed25519 private key")
	}
	pub := key.Public().(ed25519.PublicKey)
	plain := *inst
	plain.Signatures = nil
	plain.ID = ""
	data, err := Marshal(&plain)
	if err != nil {
		return err
	}
	content, err := ContentID(data)
	if err != nil {
		return err
	}
	s := Signature{
		Alg:       AlgEd25519,
		PublicKey: base64.StdEncoding.EncodeToString(pub),
		Content:   content,
		Signer:    signer,
		SignedAt:  at.UTC().Truncate(time.Second).Format(time.RFC3339),
	}
	if err := s.check0(); err != nil {
		return err
	}
	s.Value = base64.StdEncoding.EncodeToString(ed25519.Sign(key, s.Message()))
	kept := inst.Signatures[:0:0]
	for _, old := range inst.Signatures {
		if !(old.Alg == AlgEd25519 && old.PublicKey == s.PublicKey) {
			kept = append(kept, old)
		}
	}
	inst.Signatures = append(kept, s)
	return nil
}

// check0 checks a signature's fields before it is signed.
func (s *Signature) check0() error {
	if hasControl(s.Signer) || !utf8.ValidString(s.Signer) || len(s.Signer) > maxSigner {
		return fmt.Errorf("the signer name holds control characters or is over %d bytes", maxSigner)
	}
	return nil
}

// canonicalWithout is Canonical with the given top-level members removed.
func canonicalWithout(data []byte, drop ...string) ([]byte, error) {
	v, err := parseJSON(data)
	if err != nil {
		return nil, err
	}
	if v.kind != jobj {
		return nil, errors.New("not a JSON object")
	}
	gone := map[string]bool{}
	for _, d := range drop {
		gone[d] = true
		delete(v.items, d)
	}
	keys := v.keys[:0:0]
	for _, k := range v.keys {
		if !gone[k] {
			keys = append(keys, k)
		}
	}
	v.keys = keys
	var out bytes.Buffer
	writeCanonical(&out, v)
	return out.Bytes(), nil
}
