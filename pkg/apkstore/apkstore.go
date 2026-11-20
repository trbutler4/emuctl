// Package apkstore keeps APKs on the emuctl host, addressed by a key the
// client chooses (for example "android-<native fingerprint>").
//
// Entries are immutable: a key is written once and never replaced, so a key
// always names the same bytes. Writes go to a temp file and are hard-linked
// into place, so readers never see a partial APK.
package apkstore

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

var (
	ErrNotFound = errors.New("apk not found")
	ErrExists   = errors.New("apk already exists")
	ErrBadKey   = errors.New("key must match " + keyRE.String())
	ErrTooLarge = errors.New("apk must be 1 byte to 1 GiB")
)

var keyRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// MaxSize caps one APK.
const MaxSize = 1 << 30

type Store struct{ Dir string }

// Entry describes a stored APK.
type Entry struct {
	Key      string    `json:"key"`
	Bytes    int64     `json:"bytes"`
	LastUsed time.Time `json:"last_used"` // upload or last install
}

func ValidKey(key string) bool { return keyRE.MatchString(key) && !strings.Contains(key, "..") }

// Path is the file for key, or ErrNotFound.
func (s *Store) Path(key string) (string, error) {
	if !ValidKey(key) {
		return "", ErrBadKey
	}
	p := filepath.Join(s.Dir, key+".apk")
	if _, err := os.Stat(p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrNotFound
		}
		return "", err
	}
	return p, nil
}

// Touch records that key was used (installed), for listing and later eviction.
func (s *Store) Touch(key string) {
	if p, err := s.Path(key); err == nil {
		now := time.Now()
		os.Chtimes(p, now, now)
	}
}

// Put stores r under key. It fails with ErrExists if key is already stored,
// including when another upload of the same key wins a race.
func (s *Store) Put(key string, r io.Reader) (int64, error) {
	if !ValidKey(key) {
		return 0, ErrBadKey
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return 0, err
	}
	final := filepath.Join(s.Dir, key+".apk")
	if _, err := os.Stat(final); err == nil {
		return 0, ErrExists
	}
	tmp, err := os.CreateTemp(s.Dir, ".upload-*")
	if err != nil {
		return 0, err
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(r, MaxSize+1))
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	if n == 0 || n > MaxSize {
		return 0, ErrTooLarge
	}
	// Link fails if final exists, so the first complete upload wins and an
	// entry is never overwritten.
	if err := os.Link(tmp.Name(), final); err != nil {
		if errors.Is(err, os.ErrExist) {
			return 0, ErrExists
		}
		return 0, fmt.Errorf("store %s: %w", key, err)
	}
	return n, nil
}

// List returns stored APKs, most recently used first.
func (s *Store) List() ([]Entry, error) {
	ents, err := os.ReadDir(s.Dir)
	if errors.Is(err, os.ErrNotExist) {
		return []Entry{}, nil
	}
	if err != nil {
		return nil, err
	}
	out := []Entry{}
	for _, e := range ents {
		key, ok := strings.CutSuffix(e.Name(), ".apk")
		if !ok || !ValidKey(key) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, Entry{Key: key, Bytes: info.Size(), LastUsed: info.ModTime()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastUsed.After(out[j].LastUsed) })
	return out, nil
}
