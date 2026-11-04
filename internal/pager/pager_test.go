package pager

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/Aliraza777/tinydoc/internal/btree"
)

func openDB(t testing.TB, path string) *DB {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func key(i int) []byte { return []byte(fmt.Sprintf("key-%08d", i)) }

// verify compares the database with the model and checks page accounting.
func verify(t testing.TB, db *DB, want map[string]string) {
	t.Helper()
	if err := db.Check(); err != nil {
		t.Fatal(err)
	}
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	err := db.View(func(tr *btree.Tree) error {
		i := 0
		for it := tr.Seek(nil); it.Valid(); it.Next() {
			if i >= len(keys) || string(it.Key()) != keys[i] || string(it.Value()) != want[keys[i]] {
				return fmt.Errorf("scan position %d: got %q", i, it.Key())
			}
			i++
		}
		if i != len(keys) {
			return fmt.Errorf("scan found %d keys, want %d", i, len(keys))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db := openDB(t, path)
	want := map[string]string{}
	r := rand.New(rand.NewPCG(1, 1))
	for i := range 3000 {
		k := key(r.IntN(1500))
		if i%4 == 3 {
			db.Delete(k)
			delete(want, string(k))
			continue
		}
		v := bytes.Repeat([]byte{byte(i)}, r.IntN(400))
		if err := db.Set(k, v); err != nil {
			t.Fatal(err)
		}
		want[string(k)] = string(v)
	}
	verify(t, db, want)
	before := db.Stats()
	db.Close()

	db = openDB(t, path)
	verify(t, db, want)
	if after := db.Stats(); after != before {
		t.Fatalf("stats changed across reopen: %+v -> %+v", before, after)
	}
}

func TestFreedPagesAreReused(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "db"))
	v := make([]byte, 500)
	for round := range 5 {
		for i := range 500 {
			db.Set(key(i), v)
		}
		for i := range 500 {
			db.Delete(key(i))
		}
		if round == 0 {
			continue
		}
		// After the first round the file must stop growing: deleted pages are reused.
		if p := db.Stats().Pages; p > 400 {
			t.Fatalf("round %d: file grew to %d pages", round, p)
		}
	}
	verify(t, db, map[string]string{})
}

func TestBatchAndRollback(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "db"))
	err := db.Update(func(tr *btree.Tree) error {
		for i := range 100 {
			if err := tr.Set(key(i), []byte("v")); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	tx := db.Stats().TxID

	boom := errors.New("boom")
	err = db.Update(func(tr *btree.Tree) error {
		for i := range 100 {
			tr.Delete(key(i))
		}
		tr.Set([]byte("half"), []byte("done"))
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("Update returned %v", err)
	}
	if db.Stats().TxID != tx {
		t.Fatal("a failed transaction committed")
	}
	want := map[string]string{}
	for i := range 100 {
		want[string(key(i))] = "v"
	}
	verify(t, db, want)

	// A read-only transaction can't write.
	defer func() {
		if recover() == nil {
			t.Fatal("write in View didn't panic")
		}
	}()
	db.View(func(tr *btree.Tree) error { return tr.Set([]byte("x"), nil) })
}

// Crash after the data pages are written but before the meta page: the
// previous commit must survive untouched, in memory and after reopening.
func TestCrashBeforeMeta(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db := openDB(t, path)
	want := map[string]string{}
	for i := range 300 {
		db.Set(key(i), []byte("old"))
		want[string(key(i))] = "old"
	}
	crash := errors.New("simulated crash")
	db.hookBeforeMeta = func() error { return crash }
	err := db.Update(func(tr *btree.Tree) error {
		for i := range 300 {
			tr.Set(key(i), []byte("new"))
		}
		return nil
	})
	if !errors.Is(err, crash) {
		t.Fatalf("Update returned %v", err)
	}
	verify(t, db, want)
	db.hookBeforeMeta = nil
	db.Close()

	db = openDB(t, path)
	verify(t, db, want)
	// And it keeps working.
	db.Set([]byte("after"), []byte("crash"))
	want["after"] = "crash"
	verify(t, db, want)
}

// If the newest meta page is torn, Open falls back to the previous commit.
func TestTornMetaFallsBack(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db := openDB(t, path)
	db.Set([]byte("a"), []byte("1"))
	db.Set([]byte("b"), []byte("2"))
	latest := db.Stats().TxID
	db.Close()

	f, _ := os.OpenFile(path, os.O_RDWR, 0)
	f.WriteAt([]byte("garbage"), int64(latest%2)*pageSize+20)
	f.Close()

	db = openDB(t, path)
	if got := db.Stats().TxID; got != latest-1 {
		t.Fatalf("opened at tx %d, want %d", got, latest-1)
	}
	verify(t, db, map[string]string{"a": "1"})
}

func TestTrailingPagesFromUnfinishedCommitAreDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db := openDB(t, path)
	db.Set([]byte("a"), []byte("1"))
	pages := db.Stats().Pages
	db.Close()

	f, _ := os.OpenFile(path, os.O_RDWR|os.O_APPEND, 0)
	f.Write(make([]byte, 3*pageSize+100))
	f.Close()

	db = openDB(t, path)
	verify(t, db, map[string]string{"a": "1"})
	if info, _ := os.Stat(path); info.Size() != int64(pages)*pageSize {
		t.Fatalf("file is %d bytes, want %d", info.Size(), int64(pages)*pageSize)
	}
}

func TestRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	os.WriteFile(path, bytes.Repeat([]byte("x"), 3*pageSize), 0o644)
	if _, err := Open(path); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("Open of garbage: %v", err)
	}
}

func TestConcurrentReadersAndWriter(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "db"))
	for i := range 200 {
		db.Set(key(i), []byte("0"))
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for round := 1; round <= 20; round++ {
			db.Update(func(tr *btree.Tree) error {
				for i := range 200 {
					tr.Set(key(i), []byte(fmt.Sprint(round)))
				}
				return nil
			})
		}
	}()
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				// Every reader must see one whole round: all values equal.
				db.View(func(tr *btree.Tree) error {
					first, _ := tr.Get(key(0))
					for i := range 200 {
						if v, _ := tr.Get(key(i)); !bytes.Equal(v, first) {
							t.Errorf("reader saw a half-applied update: %q vs %q", v, first)
						}
					}
					return nil
				})
			}
		}()
	}
	wg.Wait()
	if err := db.Check(); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkSet(b *testing.B) {
	db := openDB(b, filepath.Join(b.TempDir(), "db"))
	v := make([]byte, 100)
	r := rand.New(rand.NewPCG(2, 2))
	for b.Loop() {
		db.Set(key(r.IntN(1_000_000)), v)
	}
}

func BenchmarkBatchSet1000(b *testing.B) {
	db := openDB(b, filepath.Join(b.TempDir(), "db"))
	v := make([]byte, 100)
	r := rand.New(rand.NewPCG(3, 3))
	for b.Loop() {
		db.Update(func(tr *btree.Tree) error {
			for range 1000 {
				tr.Set(key(r.IntN(1_000_000)), v)
			}
			return nil
		})
	}
}

func BenchmarkGet(b *testing.B) {
	db := openDB(b, filepath.Join(b.TempDir(), "db"))
	db.Update(func(tr *btree.Tree) error {
		for i := range 100_000 {
			tr.Set(key(i), make([]byte, 100))
		}
		return nil
	})
	r := rand.New(rand.NewPCG(4, 4))
	for b.Loop() {
		db.Get(key(r.IntN(100_000)))
	}
}
