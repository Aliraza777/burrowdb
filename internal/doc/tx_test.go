package doc

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"sync"
	"testing"
)

func TestTransactionCommitsEverything(t *testing.T) {
	db, path := openDB(t)
	db.CreateIndex("users", "age")
	db.Insert("users", map[string]any{"_id": "old", "age": int64(70)})
	err := db.Transaction(func(tx *Tx) error {
		if _, err := tx.Insert("users", map[string]any{"_id": "a", "age": int64(30)}); err != nil {
			return err
		}
		if _, err := tx.InsertMany("orders", []map[string]any{{"_id": "o1", "user": "a"}, {"_id": "o2", "user": "a"}}); err != nil {
			return err
		}
		if err := tx.Replace("users", "a", map[string]any{"age": int64(31)}); err != nil {
			return err
		}
		if ok, err := tx.Delete("users", "old"); !ok || err != nil {
			return fmt.Errorf("delete: %v %v", ok, err)
		}
		// Reads inside the transaction see its own writes.
		d, err := tx.Get("users", "a")
		if err != nil || d["age"] != int64(31) {
			return fmt.Errorf("read-your-writes: %v %v", d, err)
		}
		if _, err := tx.Get("users", "old"); !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("deleted document still visible: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	db = reopen(t, db, path)
	if d, _ := db.Get("users", "a"); d["age"] != int64(31) {
		t.Fatalf("after commit: %v", d)
	}
	if n, _ := db.Count("orders"); n != 2 {
		t.Fatalf("orders = %d", n)
	}
	if n, _ := db.Count("users"); n != 1 {
		t.Fatalf("users = %d", n)
	}
	checkIndexes(t, db)
}

func TestTransactionRollsBack(t *testing.T) {
	db, _ := openDB(t)
	db.CreateIndex("users", "age")
	db.Insert("users", map[string]any{"_id": "keep", "age": int64(1)})
	stop := errors.New("changed my mind")
	err := db.Transaction(func(tx *Tx) error {
		tx.Insert("users", map[string]any{"_id": "new", "age": int64(2)})
		tx.Replace("users", "keep", map[string]any{"age": int64(99)})
		tx.InsertMany("other", []map[string]any{{"x": int64(1)}})
		return stop
	})
	if !errors.Is(err, stop) {
		t.Fatalf("Transaction returned %v", err)
	}
	if _, err := db.Get("users", "new"); !errors.Is(err, ErrNotFound) {
		t.Fatal("insert from a rolled-back transaction was kept")
	}
	if d, _ := db.Get("users", "keep"); d["age"] != int64(1) {
		t.Fatalf("replace from a rolled-back transaction was kept: %v", d)
	}
	if colls, _ := db.Collections(); len(colls) != 1 {
		t.Fatalf("collections after rollback: %v", colls)
	}
	checkIndexes(t, db)
}

// A failed write aborts the transaction even if the caller ignores the error;
// otherwise half of a failed InsertMany could be committed.
func TestFailedWriteAbortsTransaction(t *testing.T) {
	db, _ := openDB(t)
	db.Insert("c", map[string]any{"_id": "taken"})
	err := db.Transaction(func(tx *Tx) error {
		tx.Insert("c", map[string]any{"_id": "first"})
		// Writes "a" and "b", then fails on the duplicate; the error is ignored.
		tx.InsertMany("c", []map[string]any{{"_id": "a"}, {"_id": "b"}, {"_id": "taken"}})
		if _, err := tx.Insert("c", map[string]any{"_id": "after"}); err == nil {
			t.Error("a write after the failure was accepted")
		}
		return nil
	})
	if ae, ok := errors.AsType[*AbortError](err); !ok || !errors.Is(ae, ErrDuplicateID) {
		t.Fatalf("Transaction returned %v, want an AbortError wrapping ErrDuplicateID", err)
	}
	for _, id := range []string{"first", "a", "b", "after"} {
		if _, err := db.Get("c", id); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s was committed by an aborted transaction", id)
		}
	}
	// Single-operation calls return the plain error, not an AbortError.
	if _, err := db.Insert("c", map[string]any{"_id": "taken"}); !errors.Is(err, ErrDuplicateID) || errors.As(err, new(*AbortError)) {
		t.Fatalf("Insert returned %v", err)
	}
	checkPages(t, db)
}

func TestTxUnusableAfterwards(t *testing.T) {
	db, _ := openDB(t)
	var leaked *Tx
	db.Transaction(func(tx *Tx) error { leaked = tx; return nil })
	if _, err := leaked.Insert("c", map[string]any{}); !errors.Is(err, ErrTxDone) {
		t.Fatalf("Insert on a finished Tx: %v", err)
	}
}

// Concurrent transfers between accounts: every transfer reads two balances
// and writes both. The total must never change, for writers or for readers
// taking snapshots in between.
func TestConcurrentTransfersKeepTheTotal(t *testing.T) {
	db, _ := openDB(t)
	const accounts, start = 10, int64(1000)
	var seed []map[string]any
	for i := range accounts {
		seed = append(seed, map[string]any{"_id": fmt.Sprintf("acct-%d", i), "balance": start})
	}
	if _, err := db.InsertMany("bank", seed); err != nil {
		t.Fatal(err)
	}
	total := func(r *Reader) int64 {
		var sum int64
		r.Scan("bank", func(d map[string]any) bool { sum += d["balance"].(int64); return true })
		return sum
	}
	var wg sync.WaitGroup
	stopReaders := make(chan struct{})
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stopReaders:
					return
				default:
				}
				db.Read(func(r *Reader) error {
					if got := total(r); got != accounts*start {
						t.Errorf("a reader saw a total of %d", got)
					}
					return nil
				})
			}
		}()
	}
	var writers sync.WaitGroup
	for w := range 16 {
		writers.Add(1)
		go func() {
			defer writers.Done()
			r := rand.New(rand.NewPCG(uint64(w), 7))
			for range 40 {
				from, to := r.IntN(accounts), r.IntN(accounts)
				amount := int64(1 + r.IntN(50))
				err := db.Transaction(func(tx *Tx) error {
					a, err := tx.Get("bank", fmt.Sprintf("acct-%d", from))
					if err != nil {
						return err
					}
					b, err := tx.Get("bank", fmt.Sprintf("acct-%d", to))
					if err != nil {
						return err
					}
					if a["balance"].(int64) < amount {
						return errors.New("insufficient funds")
					}
					a["balance"] = a["balance"].(int64) - amount
					if from == to {
						b = a
					}
					b["balance"] = b["balance"].(int64) + amount
					if err := tx.Replace("bank", a["_id"].(string), a); err != nil {
						return err
					}
					return tx.Replace("bank", b["_id"].(string), b)
				})
				if err != nil && err.Error() != "insufficient funds" {
					t.Error(err)
				}
			}
		}()
	}
	writers.Wait()
	close(stopReaders)
	wg.Wait()
	db.Read(func(r *Reader) error {
		if got := total(r); got != accounts*start {
			t.Fatalf("final total %d, want %d", got, accounts*start)
		}
		return nil
	})
	checkPages(t, db)
}
