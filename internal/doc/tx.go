package doc

import (
	"errors"
	"fmt"

	"github.com/Aliraza777/burrowdb/internal/btree"
)

// ErrTxDone is returned when a Tx is used after its Transaction returned.
var ErrTxDone = errors.New("doc: transaction already finished")

// AbortError is returned by Transaction when a write inside it failed. It
// wraps that write's error.
type AbortError struct{ Err error }

func (e *AbortError) Error() string { return "doc: transaction aborted: " + e.Err.Error() }
func (e *AbortError) Unwrap() error { return e.Err }

// Tx is a write transaction. Reads inside it see its own writes. It commits
// when the function passed to Transaction returns nil, and otherwise nothing
// it did is kept.
type Tx struct {
	db      *DB
	t       *btree.Tree
	done    bool
	aborted error // the first failed write; it aborts the whole transaction
}

// Transaction runs fn in one atomic, durable transaction. If fn returns an
// error, panics, or any write inside it failed (even if fn ignored that
// error), nothing is committed and the error is returned.
func (db *DB) Transaction(fn func(tx *Tx) error) error {
	return db.p.Update(func(t *btree.Tree) error {
		tx := &Tx{db: db, t: t}
		defer func() { tx.done = true }()
		err := fn(tx)
		if tx.aborted != nil {
			return &AbortError{tx.aborted}
		}
		return err
	})
}

// check guards every operation.
func (tx *Tx) check() error {
	if tx.done {
		return ErrTxDone
	}
	if tx.aborted != nil {
		return fmt.Errorf("doc: transaction aborted by an earlier error: %w", tx.aborted)
	}
	return nil
}

// fail records a failed write, which aborts the transaction.
func (tx *Tx) fail(err error) error {
	if err != nil && tx.aborted == nil {
		tx.aborted = err
	}
	return err
}

// Reader reads inside the transaction, seeing its writes so far.
func (tx *Tx) Reader() *Reader { return &Reader{tx.t} }

// Get returns a document, or ErrNotFound.
func (tx *Tx) Get(coll, id string) (map[string]any, error) {
	if err := tx.check(); err != nil {
		return nil, err
	}
	return tx.Reader().Get(coll, id)
}

// Insert adds a document and returns its _id.
func (tx *Tx) Insert(coll string, d map[string]any) (string, error) {
	ids, err := tx.InsertMany(coll, []map[string]any{d})
	if err != nil {
		return "", err
	}
	return ids[0], nil
}

// InsertMany adds documents.
func (tx *Tx) InsertMany(coll string, docs []map[string]any) ([]string, error) {
	if err := tx.check(); err != nil {
		return nil, err
	}
	ids := make([]string, len(docs))
	full := make([]map[string]any, len(docs))
	encs := make([][]byte, len(docs))
	for i, d := range docs {
		var err error
		if ids[i], full[i], encs[i], err = tx.db.prepare(coll, d); err != nil {
			return nil, tx.fail(fmt.Errorf("document %d: %w", i, err))
		}
	}
	t := tx.t
	indexes, err := readIndexes(t, coll)
	if err != nil {
		return nil, tx.fail(err)
	}
	for i, id := range ids {
		if _, _, exists := readDoc(t, coll, id); exists {
			return nil, tx.fail(fmt.Errorf("document %d (_id %q): %w", i, id, ErrDuplicateID))
		}
		if err := writeDoc(t, coll, id, encs[i], 0); err != nil {
			return nil, tx.fail(err)
		}
		if err := indexDoc(t, coll, id, full[i], indexes, false); err != nil {
			return nil, tx.fail(err)
		}
	}
	if err := addCount(t, coll, int64(len(ids))); err != nil {
		return nil, tx.fail(err)
	}
	return ids, nil
}

// Replace overwrites an existing document. If d has an _id it must match id.
func (tx *Tx) Replace(coll, id string, d map[string]any) error {
	if err := tx.check(); err != nil {
		return err
	}
	if other, ok := d["_id"]; ok && other != id {
		return tx.fail(ErrIDMismatch)
	}
	withID := make(map[string]any, len(d)+1)
	for k, v := range d {
		withID[k] = v
	}
	withID["_id"] = id
	_, full, enc, err := tx.db.prepare(coll, withID)
	if err != nil {
		return tx.fail(err)
	}
	t := tx.t
	oldEnc, chunks, found := readDoc(t, coll, id)
	if !found {
		return tx.fail(ErrNotFound)
	}
	indexes, err := readIndexes(t, coll)
	if err != nil {
		return tx.fail(err)
	}
	if len(indexes) > 0 {
		old, err := Decode(oldEnc)
		if err != nil {
			return tx.fail(err)
		}
		if err := indexDoc(t, coll, id, old, indexes, true); err != nil {
			return tx.fail(err)
		}
	}
	if err := writeDoc(t, coll, id, enc, chunks); err != nil {
		return tx.fail(err)
	}
	return tx.fail(indexDoc(t, coll, id, full, indexes, false))
}

// Delete removes a document and reports whether it existed.
func (tx *Tx) Delete(coll, id string) (bool, error) {
	if err := tx.check(); err != nil {
		return false, err
	}
	if err := validCollection(coll); err != nil {
		return false, tx.fail(err)
	}
	t := tx.t
	oldEnc, _, found := readDoc(t, coll, id)
	if !found {
		return false, nil
	}
	indexes, err := readIndexes(t, coll)
	if err != nil {
		return false, tx.fail(err)
	}
	if len(indexes) > 0 {
		old, err := Decode(oldEnc)
		if err != nil {
			return false, tx.fail(err)
		}
		if err := indexDoc(t, coll, id, old, indexes, true); err != nil {
			return false, tx.fail(err)
		}
	}
	if _, err := deleteDoc(t, coll, id); err != nil {
		return false, tx.fail(err)
	}
	return true, tx.fail(addCount(t, coll, -1))
}
