// Package pager stores a copy-on-write B+tree in a single file with
// crash-safe commits.
//
// File layout, in pages of btree.PageSize bytes:
//
//	page 0, 1   meta pages, written alternately
//	page 2...   tree nodes and free-list pages
//
// A meta page holds the root of the tree, the number of pages in use, the
// head of the free-list and a transaction id, protected by a checksum.
// A commit writes new pages to locations no committed tree uses, syncs,
// then writes a meta page and syncs again. Until that meta page is on disk
// the previous one still describes an untouched, consistent tree; a torn meta
// page fails its checksum and the other slot is used.
package pager

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"os"
	"slices"
	"sync"

	"github.com/Aliraza777/burrowdb/internal/btree"
)

const pageSize = btree.PageSize

// ErrCorrupt means neither meta page is valid, or the file contradicts it.
var ErrCorrupt = errors.New("pager: file is corrupt")

// ErrClosed is returned after Close.
var ErrClosed = errors.New("pager: database is closed")

const (
	magic       = "BURROW01"
	metaSize    = 52
	flHeader    = 12 // next (8) + count (4)
	flPerPage   = (pageSize - flHeader) / 8
	firstPageID = 2
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

type meta struct {
	txid      uint64
	root      uint64
	pageCount uint64 // pages in use, including the two meta pages
	freelist  uint64 // first free-list page, 0 if none
}

func (m meta) encode() []byte {
	b := make([]byte, pageSize)
	copy(b, magic)
	binary.LittleEndian.PutUint32(b[8:], 1) // format version
	binary.LittleEndian.PutUint32(b[12:], pageSize)
	binary.LittleEndian.PutUint64(b[16:], m.txid)
	binary.LittleEndian.PutUint64(b[24:], m.root)
	binary.LittleEndian.PutUint64(b[32:], m.pageCount)
	binary.LittleEndian.PutUint64(b[40:], m.freelist)
	binary.LittleEndian.PutUint32(b[48:], crc32.Checksum(b[:48], crcTable))
	return b
}

func decodeMeta(b []byte) (meta, bool) {
	if string(b[:8]) != magic || binary.LittleEndian.Uint32(b[12:]) != pageSize ||
		binary.LittleEndian.Uint32(b[48:]) != crc32.Checksum(b[:48], crcTable) {
		return meta{}, false
	}
	return meta{
		txid:      binary.LittleEndian.Uint64(b[16:]),
		root:      binary.LittleEndian.Uint64(b[24:]),
		pageCount: binary.LittleEndian.Uint64(b[32:]),
		freelist:  binary.LittleEndian.Uint64(b[40:]),
	}, true
}

// file is the subset of *os.File the pager uses; tests wrap it to check the
// order of writes and syncs.
type file interface {
	ReadAt(b []byte, off int64) (int, error)
	WriteAt(b []byte, off int64) (int, error)
	Sync() error
	Truncate(size int64) error
	Stat() (os.FileInfo, error)
	Close() error
}

// DB is a B+tree stored in a file. Writers are serialized; readers run in
// parallel with each other but not with a writer.
type DB struct {
	mu      sync.RWMutex
	f       file
	meta    meta
	free    []uint64 // pages free to reuse
	flPages []uint64 // pages holding the current free-list

	// hookBeforeMeta lets tests simulate a crash after the data pages are
	// written but before the meta page.
	hookBeforeMeta func() error

	// Group commit: Update hands its function to one committer goroutine,
	// which runs every waiting transaction and commits them with one sync.
	reqs      chan *request
	stop      chan struct{}
	stopOnce  sync.Once
	committer sync.WaitGroup
	txCount   uint64 // transactions committed since Open
}

// Stats describe the file.
type Stats struct {
	TxID      uint64 // disk commits, including those before Open
	Pages     uint64
	FreePages int
	// Transactions committed since Open. With concurrent writers this
	// exceeds the disk commits made in that time: they shared commits.
	Transactions uint64
}

// Open opens or creates a database file.
func Open(path string) (*DB, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	return openFile(f)
}

func openFile(f file) (*DB, error) {
	db := &DB{f: f, reqs: make(chan *request), stop: make(chan struct{})}
	if err := db.load(); err != nil {
		f.Close()
		return nil, err
	}
	db.committer.Add(1)
	go db.commitLoop()
	return db, nil
}

func (db *DB) load() error {
	info, err := db.f.Stat()
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		db.meta = meta{txid: 1, pageCount: firstPageID}
		if _, err := db.f.WriteAt(db.meta.encode(), pageSize); err != nil {
			return err
		}
		return db.f.Sync()
	}

	best, found := meta{}, false
	for slot := range int64(2) {
		b := make([]byte, pageSize)
		if _, err := db.f.ReadAt(b, slot*pageSize); err != nil {
			continue
		}
		if m, ok := decodeMeta(b); ok && (!found || m.txid > best.txid) {
			best, found = m, true
		}
	}
	if !found {
		return fmt.Errorf("%w: no valid meta page", ErrCorrupt)
	}
	if int64(best.pageCount)*pageSize > info.Size() {
		return fmt.Errorf("%w: meta says %d pages, file has %d bytes", ErrCorrupt, best.pageCount, info.Size())
	}
	db.meta = best
	// Pages past pageCount belong to a commit that never finished.
	if int64(best.pageCount)*pageSize < info.Size() {
		if err := db.f.Truncate(int64(best.pageCount) * pageSize); err != nil {
			return err
		}
	}
	return db.loadFreelist()
}

func (db *DB) loadFreelist() error {
	db.free, db.flPages = nil, nil
	for ptr := db.meta.freelist; ptr != 0; {
		if ptr < firstPageID || ptr >= db.meta.pageCount || len(db.flPages) > int(db.meta.pageCount) {
			return fmt.Errorf("%w: bad free-list page %d", ErrCorrupt, ptr)
		}
		b := make([]byte, pageSize)
		if _, err := db.f.ReadAt(b, int64(ptr)*pageSize); err != nil {
			return err
		}
		db.flPages = append(db.flPages, ptr)
		n := binary.LittleEndian.Uint32(b[8:])
		if n > flPerPage {
			return fmt.Errorf("%w: free-list page %d claims %d entries", ErrCorrupt, ptr, n)
		}
		for i := range n {
			db.free = append(db.free, binary.LittleEndian.Uint64(b[flHeader+8*i:]))
		}
		ptr = binary.LittleEndian.Uint64(b)
	}
	return nil
}

// pageError carries an I/O error out of the btree, whose Pages interface
// has no error results; View and Update turn it back into an error.
type pageError struct{ err error }

func (db *DB) readPage(ptr uint64) []byte {
	if ptr < firstPageID || ptr >= db.meta.pageCount {
		panic(pageError{fmt.Errorf("%w: page %d out of range", ErrCorrupt, ptr)})
	}
	b := make([]byte, pageSize)
	if _, err := db.f.ReadAt(b, int64(ptr)*pageSize); err != nil {
		panic(pageError{err})
	}
	return b
}

func catch(err *error) {
	if r := recover(); r != nil {
		pe, ok := r.(pageError)
		if !ok {
			panic(r)
		}
		*err = pe.err
	}
}

type readPages struct{ db *DB }

func (r readPages) Get(ptr uint64) []byte { return r.db.readPage(ptr) }
func (readPages) New([]byte) uint64       { panic("pager: write in a read-only transaction") }
func (readPages) Del(uint64)              { panic("pager: write in a read-only transaction") }

// View runs fn with a read-only view of the tree.
func (db *DB) View(fn func(t *btree.Tree) error) (err error) {
	db.mu.RLock()
	defer db.mu.RUnlock()
	if db.f == nil {
		return ErrClosed
	}
	defer catch(&err)
	return fn(&btree.Tree{Root: db.meta.root, Pages: readPages{db}})
}

// tx collects the page changes of a batch of write transactions in memory.
// Every allocation and free is journaled, so one transaction in the batch can
// be rolled back without touching the others.
type tx struct {
	db        *DB
	pending   map[uint64][]byte
	free      []uint64 // reusable now
	freed     []uint64 // committed pages released by this tx; reusable after commit
	pageCount uint64
	journal   []undo
}

type undoKind byte

const (
	undoNewFromFree  undoKind = iota + 1 // New took a page off the free list
	undoNewAtEnd                         // New grew the file
	undoDelPending                       // Del freed a page allocated in this batch
	undoDelCommitted                     // Del released a page of the committed tree
)

type undo struct {
	kind undoKind
	ptr  uint64
	page []byte // for undoDelPending
}

// mark returns a savepoint for rollback.
func (t *tx) mark() int { return len(t.journal) }

// rollback undoes every New and Del since the savepoint, newest first, so the
// free lists and page count return exactly to their state at the savepoint.
func (t *tx) rollback(mark int) {
	for i := len(t.journal) - 1; i >= mark; i-- {
		u := t.journal[i]
		switch u.kind {
		case undoNewFromFree:
			delete(t.pending, u.ptr)
			t.free = append(t.free, u.ptr)
		case undoNewAtEnd:
			delete(t.pending, u.ptr)
			t.pageCount--
		case undoDelPending:
			t.free = t.free[:len(t.free)-1]
			t.pending[u.ptr] = u.page
		case undoDelCommitted:
			t.freed = t.freed[:len(t.freed)-1]
		}
	}
	t.journal = t.journal[:mark]
}

func (t *tx) Get(ptr uint64) []byte {
	if p, ok := t.pending[ptr]; ok {
		return p
	}
	return t.db.readPage(ptr)
}

func (t *tx) New(page []byte) uint64 {
	var ptr uint64
	if n := len(t.free); n > 0 {
		ptr, t.free = t.free[n-1], t.free[:n-1]
		t.journal = append(t.journal, undo{kind: undoNewFromFree, ptr: ptr})
	} else {
		ptr = t.pageCount
		t.pageCount++
		t.journal = append(t.journal, undo{kind: undoNewAtEnd, ptr: ptr})
	}
	t.pending[ptr] = page
	return ptr
}

func (t *tx) Del(ptr uint64) {
	if page, ok := t.pending[ptr]; ok {
		// Never committed, so nothing on disk refers to it.
		delete(t.pending, ptr)
		t.free = append(t.free, ptr)
		t.journal = append(t.journal, undo{kind: undoDelPending, ptr: ptr, page: page})
		return
	}
	// Still part of the last committed tree until this tx commits.
	t.freed = append(t.freed, ptr)
	t.journal = append(t.journal, undo{kind: undoDelCommitted, ptr: ptr})
}

// Update runs fn in a write transaction and commits if fn returns nil; it
// returns once the commit is on disk. If fn returns an error or panics,
// nothing it did is kept (a panic is re-raised in the caller).
//
// Transactions arriving while a commit is in progress are committed together
// with a single sync. Each still sees the effects of the ones before it in
// the batch and succeeds or fails on its own.
func (db *DB) Update(fn func(t *btree.Tree) error) error {
	req := &request{fn: fn, done: make(chan result, 1)}
	select {
	case db.reqs <- req:
	case <-db.stop:
		return ErrClosed
	}
	r := <-req.done
	if r.panicked {
		panic(r.panicValue)
	}
	return r.err
}

func (db *DB) commit(t *tx, root uint64) error {
	for ptr, p := range t.pending {
		if _, err := db.f.WriteAt(p, int64(ptr)*pageSize); err != nil {
			return err
		}
	}

	free, flPages, err := db.writeFreelist(t)
	if err != nil {
		return err
	}
	if err := db.f.Sync(); err != nil {
		return err
	}
	if db.hookBeforeMeta != nil {
		if err := db.hookBeforeMeta(); err != nil {
			return err
		}
	}

	m := meta{txid: db.meta.txid + 1, root: root, pageCount: t.pageCount}
	if len(flPages) > 0 {
		m.freelist = flPages[0]
	}
	if _, err := db.f.WriteAt(m.encode(), int64(m.txid%2)*pageSize); err != nil {
		return err
	}
	if err := db.f.Sync(); err != nil {
		return err
	}
	db.meta, db.free, db.flPages = m, free, flPages
	return nil
}

// writeFreelist stores the new free-list and returns it with the pages it
// occupies. Its pages come from the free pages that are safe to overwrite now
// (t.free), so the file doesn't grow on every commit. Pages released by this
// commit (t.freed, and the old free-list pages) are still referenced by the
// last committed meta, so they are only listed, never written to.
func (db *DB) writeFreelist(t *tx) (free, ids []uint64, err error) {
	reusable := t.free
	later := slices.Concat(t.freed, db.flPages)
	// Each page taken from reusable shortens the list it has to hold; find
	// the smallest page count that fits what's left.
	k := 0
	for {
		entries := len(reusable) - min(k, len(reusable)) + len(later)
		need := (entries + flPerPage - 1) / flPerPage
		if need <= k {
			break
		}
		k = need
	}
	take := min(k, len(reusable))
	ids = slices.Clone(reusable[len(reusable)-take:])
	for len(ids) < k {
		ids = append(ids, t.pageCount)
		t.pageCount++
	}
	free = slices.Concat(reusable[:len(reusable)-take], later)
	slices.Sort(free)

	n := len(ids)
	for i, id := range ids {
		chunk := free[min(len(free), i*flPerPage):min(len(free), (i+1)*flPerPage)]
		b := make([]byte, pageSize)
		if i+1 < n {
			binary.LittleEndian.PutUint64(b, ids[i+1])
		}
		binary.LittleEndian.PutUint32(b[8:], uint32(len(chunk)))
		for j, p := range chunk {
			binary.LittleEndian.PutUint64(b[flHeader+8*j:], p)
		}
		if _, err := db.f.WriteAt(b, int64(id)*pageSize); err != nil {
			return nil, nil, err
		}
	}
	return free, ids, nil
}

// Get returns the value for key.
func (db *DB) Get(key []byte) (val []byte, ok bool, err error) {
	err = db.View(func(t *btree.Tree) error {
		val, ok = t.Get(key)
		return nil
	})
	return val, ok, err
}

// Set stores key = val in its own transaction.
func (db *DB) Set(key, val []byte) error {
	return db.Update(func(t *btree.Tree) error { return t.Set(key, val) })
}

// Delete removes key in its own transaction and reports whether it existed.
func (db *DB) Delete(key []byte) (existed bool, err error) {
	err = db.Update(func(t *btree.Tree) error {
		existed, err = t.Delete(key)
		return err
	})
	return existed, err
}

// Stats reports the file's size in pages and its free pages.
func (db *DB) Stats() Stats {
	db.mu.RLock()
	defer db.mu.RUnlock()
	return Stats{TxID: db.meta.txid, Pages: db.meta.pageCount, FreePages: len(db.free), Transactions: db.txCount}
}

// Check verifies the tree's structure and that every page is accounted for
// exactly once: meta, tree node, free, or free-list.
func (db *DB) Check() error {
	return db.View(func(t *btree.Tree) error {
		owner := make([]string, db.meta.pageCount)
		owner[0], owner[1] = "meta", "meta"
		claim := func(p uint64, what string) error {
			if p >= uint64(len(owner)) {
				return fmt.Errorf("%s page %d is past the end (%d pages)", what, p, len(owner))
			}
			if owner[p] != "" {
				return fmt.Errorf("page %d is both %s and %s", p, owner[p], what)
			}
			owner[p] = what
			return nil
		}
		var claimErr error
		if err := t.Verify(func(p uint64) {
			if err := claim(p, "tree"); err != nil && claimErr == nil {
				claimErr = err
			}
		}); err != nil {
			return err
		}
		if claimErr != nil {
			return claimErr
		}
		for _, p := range db.free {
			if err := claim(p, "free"); err != nil {
				return err
			}
		}
		for _, p := range db.flPages {
			if err := claim(p, "free-list"); err != nil {
				return err
			}
		}
		for p, o := range owner {
			if o == "" {
				return fmt.Errorf("page %d is leaked: not in the tree or the free-list", p)
			}
		}
		return nil
	})
}

// Close closes the file.
func (db *DB) Close() error {
	db.stopOnce.Do(func() { close(db.stop) })
	db.committer.Wait() // finishes the batch in progress
	db.mu.Lock()
	defer db.mu.Unlock()
	if db.f == nil {
		return nil
	}
	err := db.f.Close()
	db.f = nil
	return err
}
