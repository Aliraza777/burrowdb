package kv

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func open(t *testing.T, path string) *Store {
	t.Helper()
	s, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustGet(t *testing.T, s *Store, key, want string) {
	t.Helper()
	got, ok, err := s.Get([]byte(key))
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatalf("Get(%q): missing, want %q", key, want)
	}
	if string(got) != want {
		t.Fatalf("Get(%q) = %q, want %q", key, got, want)
	}
}

func mustMiss(t *testing.T, s *Store, key string) {
	t.Helper()
	if _, ok, err := s.Get([]byte(key)); err != nil || ok {
		t.Fatalf("Get(%q): ok=%v err=%v, want missing", key, ok, err)
	}
}

func TestSetGetDeleteSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.log")
	s := open(t, path)
	for _, kv := range [][2]string{{"a", "1"}, {"b", "2"}, {"a", "3"}, {"c", ""}} {
		if err := s.Set([]byte(kv[0]), []byte(kv[1])); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Delete([]byte("b")); err != nil {
		t.Fatal(err)
	}
	if err := s.Delete([]byte("never-existed")); err != nil {
		t.Fatal(err)
	}
	mustGet(t, s, "a", "3")
	mustGet(t, s, "c", "")
	mustMiss(t, s, "b")
	s.Close()

	s = open(t, path)
	mustGet(t, s, "a", "3")
	mustGet(t, s, "c", "")
	mustMiss(t, s, "b")
	if got := fmt.Sprint(s.Keys()); got != "[a c]" {
		t.Fatalf("Keys() = %s", got)
	}
	st := s.Stats()
	if st.Keys != 2 || st.Recovered != 0 || st.DeadBytes == 0 {
		t.Fatalf("Stats() = %+v", st)
	}
}

func TestRejectsInvalidInput(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "db.log"))
	cases := []struct {
		key, val []byte
		want     error
	}{
		{nil, []byte("v"), ErrEmptyKey},
		{make([]byte, MaxKeySize+1), nil, ErrKeyTooLarge},
		{[]byte("k"), make([]byte, MaxValueSize+1), ErrValueTooLarge},
	}
	for _, c := range cases {
		if err := s.Set(c.key, c.val); !errors.Is(err, c.want) {
			t.Errorf("Set: got %v, want %v", err, c.want)
		}
	}
	if s.Stats().FileSize != 0 {
		t.Fatal("rejected writes reached the file")
	}
}

// A crash during an append can leave any prefix of the last record on disk.
// Every such prefix must be cut off, keeping all earlier records.
func TestRecoversFromTornLastRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.log")
	s := open(t, path)
	s.Set([]byte("one"), []byte("first"))
	s.Set([]byte("two"), []byte("second"))
	good := s.Stats().FileSize
	s.Set([]byte("three"), []byte("third value"))
	full := s.Stats().FileSize
	s.Close()
	whole, _ := os.ReadFile(path)

	for cut := good; cut < full; cut++ {
		os.WriteFile(path, whole[:cut], 0o644)
		s := open(t, path)
		mustGet(t, s, "one", "first")
		mustGet(t, s, "two", "second")
		mustMiss(t, s, "three")
		if st := s.Stats(); st.Recovered != cut-good || st.FileSize != good {
			t.Fatalf("cut at %d: Stats() = %+v", cut, st)
		}
		// The store keeps working after recovery, and the new write persists.
		if err := s.Set([]byte("four"), []byte("after")); err != nil {
			t.Fatal(err)
		}
		s.Close()
		s = open(t, path)
		mustGet(t, s, "four", "after")
		mustGet(t, s, "two", "second")
		s.Close()
	}
}

func TestDropsCorruptOrGarbageTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db.log")
	s := open(t, path)
	s.Set([]byte("keep"), []byte("me"))
	good := s.Stats().FileSize
	s.Set([]byte("flip"), []byte("this value gets a bit flipped"))
	s.Close()

	data, _ := os.ReadFile(path)
	data[len(data)-3] ^= 0x40 // one bit inside the last value
	os.WriteFile(path, data, 0o644)
	s = open(t, path)
	mustGet(t, s, "keep", "me")
	mustMiss(t, s, "flip")
	if s.Stats().FileSize != good {
		t.Fatalf("corrupt record not cut: %+v", s.Stats())
	}
	s.Close()

	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	f.Write(bytes.Repeat([]byte{0xff}, 37))
	f.Close()
	s = open(t, path)
	mustGet(t, s, "keep", "me")
	if st := s.Stats(); st.Recovered != 37 {
		t.Fatalf("garbage tail: Stats() = %+v", st)
	}
}

func TestCompactReclaimsSpaceAndKeepsData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.log")
	s := open(t, path)
	for i := range 200 {
		s.Set([]byte(fmt.Sprintf("k%03d", i%20)), []byte(fmt.Sprintf("v%d", i)))
	}
	s.Delete([]byte("k000"))
	before := s.Stats()
	if err := s.Compact(); err != nil {
		t.Fatal(err)
	}
	after := s.Stats()
	if after.DeadBytes != 0 || after.FileSize >= before.FileSize || after.Keys != 19 {
		t.Fatalf("before %+v, after %+v", before, after)
	}
	mustGet(t, s, "k019", "v199")
	mustMiss(t, s, "k000")
	// Writes after compaction go to the new file.
	s.Set([]byte("new"), []byte("x"))
	s.Close()

	s = open(t, path)
	mustGet(t, s, "k019", "v199")
	mustGet(t, s, "new", "x")
	mustMiss(t, s, "k000")
	if _, err := os.Stat(path + ".compact"); !os.IsNotExist(err) {
		t.Fatal("temporary compaction file left behind")
	}
}

func TestOpenDiscardsInterruptedCompaction(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "db.log")
	s := open(t, path)
	s.Set([]byte("a"), []byte("1"))
	s.Close()
	os.WriteFile(path+".compact", []byte("half-written compaction"), 0o644)

	s = open(t, path)
	mustGet(t, s, "a", "1")
	if _, err := os.Stat(path + ".compact"); !os.IsNotExist(err) {
		t.Fatal("stale compaction file not removed")
	}
}

func TestClosedStore(t *testing.T) {
	s := open(t, filepath.Join(t.TempDir(), "db.log"))
	s.Close()
	if err := s.Set([]byte("a"), nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Set after Close: %v", err)
	}
	if _, _, err := s.Get([]byte("a")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Get after Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal("second Close should be a no-op")
	}
}

// Run with -race: readers and writers in parallel, plus a compaction.
func TestConcurrentAccess(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "db.log"), Options{NoSync: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 300 {
				key := []byte(fmt.Sprintf("w%d-%d", w, i%50))
				if err := s.Set(key, []byte(fmt.Sprint(i))); err != nil {
					t.Error(err)
					return
				}
				if _, _, err := s.Get(key); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for range 5 {
			if err := s.Compact(); err != nil {
				t.Error(err)
			}
		}
	}()
	wg.Wait()
	for w := range 4 {
		for k := range 50 {
			key := fmt.Sprintf("w%d-%d", w, k)
			mustGet(t, s, key, fmt.Sprint(250+k))
		}
	}
}
