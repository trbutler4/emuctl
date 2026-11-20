package apkstore

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestPutIsImmutable(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	if _, err := s.Put("android-abc", strings.NewReader("one")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Put("android-abc", strings.NewReader("two")); !errors.Is(err, ErrExists) {
		t.Fatalf("second put: %v, want ErrExists", err)
	}
	p, err := s.Path("android-abc")
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(p); string(b) != "one" {
		t.Fatalf("entry was overwritten: %q", b)
	}
}

func TestConcurrentPutOneWinner(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.Put("k", strings.NewReader("apk")); err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			} else if !errors.Is(err, ErrExists) {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d uploads won, want 1", wins)
	}
	// No temp files are left behind.
	ents, _ := os.ReadDir(s.Dir)
	if len(ents) != 1 {
		t.Fatalf("store dir has %d entries, want 1", len(ents))
	}
}

func TestBadKeysAndSizes(t *testing.T) {
	s := &Store{Dir: t.TempDir()}
	for _, k := range []string{"", "../x", "a/b", ".hidden", "a..b", strings.Repeat("a", 200)} {
		if _, err := s.Put(k, strings.NewReader("x")); !errors.Is(err, ErrBadKey) {
			t.Errorf("key %q: %v, want ErrBadKey", k, err)
		}
	}
	if _, err := s.Put("empty", strings.NewReader("")); !errors.Is(err, ErrTooLarge) {
		t.Errorf("empty apk: %v", err)
	}
	if _, err := s.Path("missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing: %v", err)
	}
}

func TestList(t *testing.T) {
	s := &Store{Dir: filepath.Join(t.TempDir(), "not-yet")}
	if l, err := s.List(); err != nil || len(l) != 0 {
		t.Fatalf("empty store: %v %v", l, err)
	}
	s.Put("a", strings.NewReader("12345"))
	l, err := s.List()
	if err != nil || len(l) != 1 || l[0].Key != "a" || l[0].Bytes != 5 {
		t.Fatalf("list: %+v %v", l, err)
	}
}
