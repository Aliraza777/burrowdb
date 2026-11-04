package pager

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Aliraza777/burrowdb/internal/btree"
)

// runAsBatch commits fns as one batch, the way the committer would, and
// returns each one's result.
func runAsBatch(db *DB, fns ...func(t *btree.Tree) error) []result {
	batch := make([]*request, len(fns))
	for i, fn := range fns {
		batch[i] = &request{fn: fn, done: make(chan result, 1)}
	}
	db.runBatch(batch)
	out := make([]result, len(fns))
	for i, req := range batch {
		out[i] = <-req.done
	}
	return out
}

func setN(prefix string, n int, then error) func(t *btree.Tree) error {
	return func(t *btree.Tree) error {
		for i := range n {
			if err := t.Set([]byte(fmt.Sprintf("%s-%04d", prefix, i)), bytes.Repeat([]byte("v"), 300)); err != nil {
				return err
			}
		}
		return then
	}
}

func TestBatchKeepsOnlySuccessfulTransactions(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "db"))
	db.Set([]byte("existing"), []byte("x"))
	before := db.Stats().TxID
	boom := errors.New("boom")
	res := runAsBatch(db,
		setN("ok1", 200, nil),
		setN("fail", 200, boom),
		func(t *btree.Tree) error { // deletes committed data, then fails
			t.Delete([]byte("existing"))
			setN("fail2", 50, nil)(t)
			return boom
		},
		setN("ok2", 200, nil),
		func(t *btree.Tree) error { setN("panic", 100, nil)(t); panic("kaboom") },
		func(t *btree.Tree) error { // sees ok1 and ok2, which came earlier in the batch
			if _, ok := t.Get([]byte("ok2-0000")); !ok {
				return errors.New("earlier transaction in the batch not visible")
			}
			return t.Set([]byte("ok3"), []byte("y"))
		},
	)
	wantErr := []error{nil, boom, boom, nil, nil, nil}
	for i, r := range res {
		if !errors.Is(r.err, wantErr[i]) {
			t.Errorf("transaction %d: err %v, want %v", i, r.err, wantErr[i])
		}
	}
	if !res[4].panicked || res[4].panicValue != "kaboom" {
		t.Fatalf("panic not reported to its caller: %+v", res[4])
	}
	if got := db.Stats().TxID - before; got != 1 {
		t.Fatalf("batch made %d disk commits, want 1", got)
	}
	err := db.View(func(tr *btree.Tree) error {
		for _, k := range []string{"ok1-0199", "ok2-0000", "ok3", "existing"} {
			if _, ok := tr.Get([]byte(k)); !ok {
				t.Errorf("%s missing", k)
			}
		}
		for _, k := range []string{"fail-0000", "fail2-0000", "panic-0000"} {
			if _, ok := tr.Get([]byte(k)); ok {
				t.Errorf("%s from a failed transaction was kept", k)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Check(); err != nil {
		t.Fatalf("rollbacks leaked pages: %v", err)
	}

	// Through the public API, a panic reaches the caller's goroutine.
	defer func() {
		if r := recover(); r != "public kaboom" {
			t.Fatalf("recovered %v", r)
		}
	}()
	db.Update(func(*btree.Tree) error { panic("public kaboom") })
}

// Random batches with random failures, against a model. Failed transactions
// often delete pages that earlier transactions in the same batch allocated,
// which is the hardest case for the undo journal.
func TestRandomBatchesAgainstModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "db")
	db := openDB(t, path)
	r := rand.New(rand.NewPCG(31, 32))
	want := map[string]string{}
	type change struct {
		key string
		val *string
	}
	for round := range 300 {
		n := 1 + r.IntN(8)
		var fns []func(*btree.Tree) error
		var plans [][]change
		var fails []bool
		for range n {
			var ch []change
			for range r.IntN(40) {
				k := fmt.Sprintf("k%03d", r.IntN(300))
				if r.IntN(3) == 0 {
					ch = append(ch, change{k, nil})
				} else {
					v := fmt.Sprintf("%d-%s", round, bytes.Repeat([]byte("x"), r.IntN(500)))
					ch = append(ch, change{k, &v})
				}
			}
			fail := r.IntN(3) == 0
			plans, fails = append(plans, ch), append(fails, fail)
			fns = append(fns, func(t *btree.Tree) error {
				for _, c := range ch {
					if c.val == nil {
						t.Delete([]byte(c.key))
					} else if err := t.Set([]byte(c.key), []byte(*c.val)); err != nil {
						return err
					}
				}
				if fail {
					return errors.New("planned failure")
				}
				return nil
			})
		}
		res := runAsBatch(db, fns...)
		for i, rr := range res {
			if (rr.err != nil) != fails[i] {
				t.Fatalf("round %d tx %d: err %v, planned failure %v", round, i, rr.err, fails[i])
			}
			if fails[i] {
				continue
			}
			for _, c := range plans[i] {
				if c.val == nil {
					delete(want, c.key)
				} else {
					want[c.key] = *c.val
				}
			}
		}
		if round%50 == 49 {
			verify(t, db, want)
			db.Close()
			db = openDB(t, path)
		}
	}
	verify(t, db, want)
}

// Many goroutines writing at once share commits, and every write lands.
func TestConcurrentWritersShareCommits(t *testing.T) {
	db := openDB(t, filepath.Join(t.TempDir(), "db"))
	const writers, each = 32, 25
	before := db.Stats().TxID
	start := time.Now()
	var wg sync.WaitGroup
	for w := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range each {
				if err := db.Set([]byte(fmt.Sprintf("w%02d-%03d", w, i)), []byte("v")); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	commits := db.Stats().TxID - before
	want := map[string]string{}
	for w := range writers {
		for i := range each {
			want[fmt.Sprintf("w%02d-%03d", w, i)] = "v"
		}
	}
	verify(t, db, want)
	if commits*4 > writers*each {
		t.Fatalf("%d transactions took %d disk commits: not batching", writers*each, commits)
	}
	t.Logf("%d transactions from %d goroutines in %v: %d disk commits (%.0f tx/s)",
		writers*each, writers, elapsed.Round(time.Millisecond), commits, float64(writers*each)/elapsed.Seconds())
}

func TestUpdateAfterClose(t *testing.T) {
	db, err := Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	db.Close()
	if err := db.Set([]byte("a"), nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Set after Close: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal("second Close should be a no-op")
	}
}
