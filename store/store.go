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

// Store is a folder of instances named by their ids. It keeps an index in
// memory, built when the store is opened and added to on every Put; files
// put in the folder by other means are seen when the store is opened again.
type Store struct {
	dir   string
	mu    sync.Mutex
	index map[string]indexed // by hex id
}

type indexed struct {
	entry Entry
	fp    codeinstances.Fingerprint
}

// Open opens the store in dir, creating the folder if it isn't there, and
// reads every instance in it. Files that aren't intact instances under
// their own id, or aren't regular files, are left out.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, index: map[string]indexed{}}
	ids, err := s.fileIDs()
	if err != nil {
		return nil, err
	}
	for _, hex := range ids {
		data, err := s.read(hex)
		if err != nil {
			continue
		}
		if inst, err := instance.Parse(data); err == nil && inst.ID == "sha256:"+hex {
			s.add(hex, inst, int64(len(data)))
		}
	}
	return s, nil
}

// Dir is the store's folder.
func (s *Store) Dir() string { return s.dir }

func (s *Store) add(hex string, inst *instance.Instance, size int64) {
	e := Entry{ID: inst.ID, Created: inst.Created, Command: inst.Command.Argv, Ended: codeinstances.Ended(inst.Result),
		DurationMS: inst.Result.DurationMS, Calls: len(inst.Calls), Values: len(inst.Values), Bytes: size,
		Failed: codeinstances.Failed(inst)}
	var fp codeinstances.Fingerprint
	if e.Failed {
		fp = codeinstances.FingerprintOf(inst)
		e.Fingerprint = fp.ID
	}
	s.index[hex] = indexed{e, fp}
}

// read reads a stored file, which has to be a regular file.
func (s *Store) read(hex string) ([]byte, error) {
	path := filepath.Join(s.dir, hex+".instance")
	fi, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s isn't a regular file", path)
	}
	return os.ReadFile(path)
}

// Put checks that data is an intact instance and keeps it. It returns the
// instance's id and whether it was new to the store.
func (s *Store) Put(data []byte) (string, bool, error) {
	inst, err := instance.Parse(data)
	if err != nil {
		return "", false, err
	}
	hex := strings.TrimPrefix(inst.ID, "sha256:")
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.index[hex]; ok {
		if stored, err := s.read(hex); err == nil {
			if id, err := instance.ComputeID(stored); err == nil && id == inst.ID && instance.VerifyBytes(stored) == nil {
				return inst.ID, false, nil
			}
		}
		// The stored copy was changed or removed behind the store's back.
	}
	// Whatever is at the path isn't an intact copy (or it would be in the
	// index), so it is replaced.
	if err := instance.WriteAtomic(filepath.Join(s.dir, hex+".instance"), data); err != nil {
		return "", false, err
	}
	s.add(hex, inst, int64(len(data)))
	return inst.ID, true, nil
}

// Resolve turns a full id or a unique prefix into the full id.
func (s *Store) Resolve(ref string) (string, error) {
	hex, err := cleanRef(ref)
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	found := ""
	for id := range s.index {
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

// ErrDamaged means a stored file no longer matches its id.
var ErrDamaged = errors.New("the stored file was changed or removed")

// Get returns a stored instance's bytes and its full id, checked again.
func (s *Store) Get(ref string) ([]byte, string, error) {
	id, err := s.Resolve(ref)
	if err != nil {
		return nil, "", err
	}
	data, err := s.read(strings.TrimPrefix(id, "sha256:"))
	if err != nil {
		return nil, "", ErrDamaged
	}
	if err := instance.VerifyBytes(data); err != nil {
		return nil, "", ErrDamaged
	}
	if got, _ := instance.ComputeID(data); got != id {
		return nil, "", ErrDamaged
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

// List describes every stored instance, newest first.
func (s *Store) List() []Entry {
	s.mu.Lock()
	entries := make([]Entry, 0, len(s.index))
	for _, ix := range s.index {
		entries = append(entries, ix.entry)
	}
	s.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool {
		if !entries[i].Created.Equal(entries[j].Created) {
			return entries[i].Created.After(entries[j].Created)
		}
		return entries[i].ID < entries[j].ID
	})
	return entries
}

// Groups groups the stored failing instances by fingerprint, naming each
// run by its id.
func (s *Store) Groups() *codeinstances.Grouping {
	s.mu.Lock()
	runs := make([]codeinstances.FingerprintedRun, 0, len(s.index))
	for _, ix := range s.index {
		runs = append(runs, codeinstances.FingerprintedRun{Name: ix.entry.ID, Created: ix.entry.Created, Failed: ix.entry.Failed, Fingerprint: ix.fp})
	}
	s.mu.Unlock()
	sort.Slice(runs, func(i, j int) bool { return runs[i].Name < runs[j].Name })
	return codeinstances.GroupFingerprints(runs)
}

// fileIDs lists the hex ids of the files in the store's folder, sorted.
func (s *Store) fileIDs() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, e := range entries {
		hex, ok := strings.CutSuffix(e.Name(), ".instance")
		if ok && len(hex) == 64 && isHex(hex) {
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
