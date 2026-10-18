// Copyright CodeInstances.com 2026
// SPDX-License-Identifier: Apache-2.0

// Package store keeps instances in a folder under their ids, serves them
// over HTTP, and pushes to and pulls from such a server.
//
// An instance's id, the SHA-256 of its canonical form, is its address: a
// store only takes a file whose id checks out, keeps it as <hex>.instance,
// and finds it again by the full id or any unique prefix of at least 12 hex
// digits. The same file pushed twice is stored once.
package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codeinstances/codeinstances"
	"github.com/codeinstances/codeinstances/instance"
)

// MinPrefix is the shortest id prefix a store looks up.
const MinPrefix = 12

var (
	// ErrNotFound means no stored instance has the id.
	ErrNotFound = errors.New("no instance with that id")
	// ErrAmbiguous means more than one stored instance starts with the prefix.
	ErrAmbiguous = errors.New("more than one instance starts with that id")
	// ErrBadRef means the id isn't one: hex digits, at least MinPrefix of
	// them, with or without "sha256:" in front.
	ErrBadRef = fmt.Errorf("an id is %d to 64 hex digits, with or without sha256: in front", MinPrefix)
)

// Store is a folder of instances named by their ids.
type Store struct {
	dir string
	mu  sync.Mutex
}

// Open opens the store in dir, creating the folder if it isn't there.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// Dir is the store's folder.
func (s *Store) Dir() string { return s.dir }

// Put checks that data is an intact instance and keeps it. It returns the
// instance's id and whether it was new to the store.
func (s *Store) Put(data []byte) (string, bool, error) {
	inst, err := instance.Parse(data)
	if err != nil {
		return "", false, err
	}
	hex := strings.TrimPrefix(inst.ID, "sha256:")
	path := filepath.Join(s.dir, hex+".instance")
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := os.Stat(path); err == nil {
		return inst.ID, false, nil
	}
	if err := instance.WriteAtomic(path, data); err != nil {
		return "", false, err
	}
	return inst.ID, true, nil
}

// Resolve turns a full id or a unique prefix into the full id.
func (s *Store) Resolve(ref string) (string, error) {
	hex, err := cleanRef(ref)
	if err != nil {
		return "", err
	}
	if len(hex) == 64 {
		if _, err := os.Stat(filepath.Join(s.dir, hex+".instance")); err != nil {
			return "", ErrNotFound
		}
		return "sha256:" + hex, nil
	}
	ids, err := s.ids()
	if err != nil {
		return "", err
	}
	found := ""
	for _, id := range ids {
		if strings.HasPrefix(id, hex) {
			if found != "" {
				return "", ErrAmbiguous
			}
			found = id
		}
	}
	if found == "" {
		return "", ErrNotFound
	}
	return "sha256:" + found, nil
}

// Get returns a stored instance's bytes and its full id.
func (s *Store) Get(ref string) ([]byte, string, error) {
	id, err := s.Resolve(ref)
	if err != nil {
		return nil, "", err
	}
	data, err := os.ReadFile(filepath.Join(s.dir, strings.TrimPrefix(id, "sha256:")+".instance"))
	if err != nil {
		return nil, "", err
	}
	return data, id, nil
}

// Entry describes a stored instance.
type Entry struct {
	ID          string    `json:"id"`
	Created     time.Time `json:"created"`
	Command     []string  `json:"command"`
	Ended       string    `json:"ended"`
	DurationMS  int64     `json:"duration_ms"`
	Calls       int       `json:"calls"`
	Values      int       `json:"values"`
	Bytes       int64     `json:"bytes"`
	Failed      bool      `json:"failed"`
	Fingerprint string    `json:"fingerprint,omitempty"`
}

// List describes every stored instance, newest first. Files that don't
// read as instances are skipped.
func (s *Store) List() ([]Entry, error) {
	insts, err := s.all()
	if err != nil {
		return nil, err
	}
	entries := make([]Entry, 0, len(insts))
	for _, si := range insts {
		inst := si.inst
		e := Entry{ID: inst.ID, Created: inst.Created, Command: inst.Command.Argv, Ended: codeinstances.Ended(inst.Result),
			DurationMS: inst.Result.DurationMS, Calls: len(inst.Calls), Values: len(inst.Values), Bytes: si.size,
			Failed: codeinstances.Failed(inst)}
		if e.Failed {
			e.Fingerprint = codeinstances.FingerprintOf(inst).ID
		}
		entries = append(entries, e)
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if !entries[i].Created.Equal(entries[j].Created) {
			return entries[i].Created.After(entries[j].Created)
		}
		return entries[i].ID < entries[j].ID
	})
	return entries, nil
}

// Groups groups the stored failing instances by fingerprint, naming each
// run by its id.
func (s *Store) Groups() (*codeinstances.Grouping, error) {
	insts, err := s.all()
	if err != nil {
		return nil, err
	}
	runs := make([]codeinstances.NamedInstance, len(insts))
	for i, si := range insts {
		runs[i] = codeinstances.NamedInstance{Name: si.inst.ID, Instance: si.inst}
	}
	return codeinstances.GroupFailures(runs), nil
}

type storedInstance struct {
	inst *instance.Instance
	size int64
}

func (s *Store) all() ([]storedInstance, error) {
	ids, err := s.ids()
	if err != nil {
		return nil, err
	}
	var out []storedInstance
	for _, id := range ids {
		path := filepath.Join(s.dir, id+".instance")
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		inst, err := instance.Parse(data)
		if err != nil || inst.ID != "sha256:"+id {
			continue
		}
		out = append(out, storedInstance{inst, int64(len(data))})
	}
	return out, nil
}

// ids lists the hex ids of the files in the store, sorted.
func (s *Store) ids() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		name := e.Name()
		hex, ok := strings.CutSuffix(name, ".instance")
		if ok && !e.IsDir() && len(hex) == 64 && isHex(hex) {
			ids = append(ids, hex)
		}
	}
	sort.Strings(ids)
	return ids, nil
}

func cleanRef(ref string) (string, error) {
	hex := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ref), "sha256:"))
	if len(hex) < MinPrefix || len(hex) > 64 || !isHex(hex) {
		return "", ErrBadRef
	}
	return hex, nil
}

func isHex(s string) bool {
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return s != ""
}
