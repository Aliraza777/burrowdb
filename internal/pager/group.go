package pager

import (
	"slices"

	"github.com/Aliraza777/burrowdb/internal/btree"
)

// maxBatch bounds how many transactions share one commit.
const maxBatch = 1000

type request struct {
	fn   func(t *btree.Tree) error
	done chan result
}

type result struct {
	err        error
	panicked   bool
	panicValue any
}

// commitLoop is the single writer: it takes a request, gathers every other
// request already waiting, and commits them as one batch.
func (db *DB) commitLoop() {
	defer db.committer.Done()
	for {
		select {
		case <-db.stop:
			return
		case req := <-db.reqs:
			batch := []*request{req}
		gather:
			for len(batch) < maxBatch {
				select {
				case r := <-db.reqs:
					batch = append(batch, r)
				default:
					break gather
				}
			}
			db.runBatch(batch)
		}
	}
}

func (db *DB) runBatch(batch []*request) {
	results := make([]result, len(batch))
	defer func() {
		for i, req := range batch {
			req.done <- results[i]
		}
	}()
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.f == nil {
		for i := range results {
			results[i].err = ErrClosed
		}
		return
	}
	t := &tx{db: db, pending: map[uint64][]byte{}, free: slices.Clone(db.free), pageCount: db.meta.pageCount}
	tree := &btree.Tree{Root: db.meta.root, Pages: t}
	var committed []int
	for i, req := range batch {
		mark, root := t.mark(), tree.Root
		results[i] = runOne(req.fn, tree)
		if results[i].err != nil || results[i].panicked {
			// Undo only this transaction; earlier ones in the batch stay.
			t.rollback(mark)
			tree.Root = root
			continue
		}
		committed = append(committed, i)
	}
	if len(committed) == 0 {
		return
	}
	if tree.Root == db.meta.root && len(t.pending) == 0 && len(t.freed) == 0 {
		return // nothing changed
	}
	if err := db.commit(t, tree.Root); err != nil {
		for _, i := range committed {
			results[i].err = err
		}
		return
	}
	db.txCount += uint64(len(committed))
}

// runOne calls fn, turning an I/O failure into an error and catching any
// other panic so it can be re-raised in the caller's goroutine.
func runOne(fn func(t *btree.Tree) error, tree *btree.Tree) (r result) {
	defer func() {
		if p := recover(); p != nil {
			if pe, ok := p.(pageError); ok {
				r = result{err: pe.err}
				return
			}
			r = result{panicked: true, panicValue: p}
		}
	}()
	return result{err: fn(tree)}
}
