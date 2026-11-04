// Package doc stores JSON-like documents in named collections on top of the
// B+tree pager.
//
// Everything lives in one B+tree, separated by key prefix:
//
//	c/<collection>                          collection metadata (document count)
//	d/<collection>\x00<_id>\x00<chunk BE16>  document data, in chunks
//
// A document's encoding is split into chunks of at most btree.MaxValueSize
// bytes, so documents up to MaxDocSize fit. Chunk keys sort in order, so a
// document is a contiguous key range and a collection is ordered by _id.
package doc

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Aliraza777/burrowdb/internal/btree"
	"github.com/Aliraza777/burrowdb/internal/pager"
)

// MaxDocSize is the largest encoded document, as in MongoDB.
const MaxDocSize = 16 << 20

const chunkSize = btree.MaxValueSize

var (
	ErrBadCollection = errors.New("doc: collection names are 1-64 characters of A-Z a-z 0-9 _ . -")
	ErrBadID         = errors.New("doc: _id must be a string of 1-128 printable characters without spaces")
	ErrNotFound      = errors.New("doc: document not found")
	ErrDuplicateID   = errors.New("doc: a document with this _id already exists")
	ErrTooLarge      = fmt.Errorf("doc: document larger than %d bytes", MaxDocSize)
	ErrIDMismatch    = errors.New("doc: the document's _id doesn't match the one being replaced")
)

// DB is a document database in one file.
type DB struct {
	p   *pager.DB
	ids *idGen
}

// Open opens or creates a database file.
func Open(path string) (*DB, error) {
	p, err := pager.Open(path)
	if err != nil {
		return nil, err
	}
	return &DB{p: p, ids: newIDGen()}, nil
}

// Pager exposes the storage layer, for stats and integrity checks.
func (db *DB) Pager() *pager.DB { return db.p }

// Close closes the database.
func (db *DB) Close() error { return db.p.Close() }

func validCollection(name string) error {
	if len(name) == 0 || len(name) > 64 {
		return ErrBadCollection
	}
	for _, c := range []byte(name) {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-'
		if !ok {
			return ErrBadCollection
		}
	}
	return nil
}

func validID(id string) error {
	if len(id) == 0 || len(id) > 128 {
		return ErrBadID
	}
	for _, c := range []byte(id) {
		if c < '!' || c > '~' {
			return ErrBadID
		}
	}
	return nil
}

func metaKey(coll string) []byte   { return []byte("c/" + coll) }
func docPrefix(coll string) []byte { return []byte("d/" + coll + "\x00") }
func idPrefix(coll, id string) []byte {
	return append(docPrefix(coll), id+"\x00"...)
}

func chunkKey(coll, id string, n int) []byte {
	return binary.BigEndian.AppendUint16(idPrefix(coll, id), uint16(n))
}

// --- operations on a tree, used inside one transaction ---

func readCount(t *btree.Tree, coll string) (int64, bool, error) {
	v, ok := t.Get(metaKey(coll))
	if !ok {
		return 0, false, nil
	}
	m, err := Decode(v)
	if err != nil {
		return 0, true, err
	}
	n, _ := m["count"].(int64)
	return n, true, nil
}

func addCount(t *btree.Tree, coll string, delta int64) error {
	n, _, err := readCount(t, coll)
	if err != nil {
		return err
	}
	enc, _ := Encode(map[string]any{"count": n + delta})
	return t.Set(metaKey(coll), enc)
}

// readDoc returns a document's encoding and how many chunks it uses.
func readDoc(t *btree.Tree, coll, id string) ([]byte, int, bool) {
	prefix := idPrefix(coll, id)
	var buf []byte
	chunks := 0
	for it := t.Seek(prefix); it.Valid() && bytes.HasPrefix(it.Key(), prefix); it.Next() {
		buf = append(buf, it.Value()...)
		chunks++
	}
	return buf, chunks, chunks > 0
}

// writeDoc stores enc in chunks, deleting chunks left over from a longer
// previous version.
func writeDoc(t *btree.Tree, coll, id string, enc []byte, oldChunks int) error {
	n := 0
	for start := 0; start < len(enc) || n == 0; start += chunkSize {
		if err := t.Set(chunkKey(coll, id, n), enc[start:min(len(enc), start+chunkSize)]); err != nil {
			return err
		}
		n++
	}
	for ; n < oldChunks; n++ {
		if _, err := t.Delete(chunkKey(coll, id, n)); err != nil {
			return err
		}
	}
	return nil
}

func deleteDoc(t *btree.Tree, coll, id string) (bool, error) {
	_, chunks, found := readDoc(t, coll, id)
	for n := range chunks {
		if _, err := t.Delete(chunkKey(coll, id, n)); err != nil {
			return false, err
		}
	}
	return found, nil
}

// prepare validates a document, assigns an _id if it has none, and encodes it.
func (db *DB) prepare(coll string, d map[string]any) (string, []byte, error) {
	if err := validCollection(coll); err != nil {
		return "", nil, err
	}
	if d == nil {
		return "", nil, errors.New("doc: nil document")
	}
	out := make(map[string]any, len(d)+1)
	for k, v := range d {
		out[k] = v
	}
	id, hasID := out["_id"]
	if !hasID {
		id = db.ids.next()
		out["_id"] = id
	}
	s, ok := id.(string)
	if !ok || validID(s) != nil {
		return "", nil, ErrBadID
	}
	enc, err := Encode(out)
	if err != nil {
		return "", nil, err
	}
	if len(enc) > MaxDocSize {
		return "", nil, ErrTooLarge
	}
	return s, enc, nil
}

// --- public API ---

// Insert adds a document, creating the collection if needed, and returns its
// _id. A document without an _id gets a generated one.
func (db *DB) Insert(coll string, d map[string]any) (string, error) {
	ids, err := db.InsertMany(coll, []map[string]any{d})
	if err != nil {
		return "", err
	}
	return ids[0], nil
}

// InsertMany adds documents in one transaction: all of them, or none.
func (db *DB) InsertMany(coll string, docs []map[string]any) ([]string, error) {
	ids := make([]string, len(docs))
	encs := make([][]byte, len(docs))
	for i, d := range docs {
		var err error
		if ids[i], encs[i], err = db.prepare(coll, d); err != nil {
			return nil, fmt.Errorf("document %d: %w", i, err)
		}
	}
	err := db.p.Update(func(t *btree.Tree) error {
		for i, id := range ids {
			if _, _, exists := readDoc(t, coll, id); exists {
				return fmt.Errorf("document %d (_id %q): %w", i, id, ErrDuplicateID)
			}
			if err := writeDoc(t, coll, id, encs[i], 0); err != nil {
				return err
			}
		}
		return addCount(t, coll, int64(len(ids)))
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// Get returns the document with the given _id.
func (db *DB) Get(coll, id string) (map[string]any, error) {
	if err := validCollection(coll); err != nil {
		return nil, err
	}
	var enc []byte
	var found bool
	err := db.p.View(func(t *btree.Tree) error {
		enc, _, found = readDoc(t, coll, id)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, ErrNotFound
	}
	return Decode(enc)
}

// Replace overwrites an existing document. If d has an _id it must match id.
func (db *DB) Replace(coll, id string, d map[string]any) error {
	if other, ok := d["_id"]; ok && other != id {
		return ErrIDMismatch
	}
	withID := make(map[string]any, len(d)+1)
	for k, v := range d {
		withID[k] = v
	}
	withID["_id"] = id
	_, enc, err := db.prepare(coll, withID)
	if err != nil {
		return err
	}
	return db.p.Update(func(t *btree.Tree) error {
		_, chunks, found := readDoc(t, coll, id)
		if !found {
			return ErrNotFound
		}
		return writeDoc(t, coll, id, enc, chunks)
	})
}

// Delete removes a document and reports whether it existed.
func (db *DB) Delete(coll, id string) (bool, error) {
	if err := validCollection(coll); err != nil {
		return false, err
	}
	var deleted bool
	err := db.p.Update(func(t *btree.Tree) error {
		var err error
		if deleted, err = deleteDoc(t, coll, id); err != nil || !deleted {
			return err
		}
		return addCount(t, coll, -1)
	})
	return deleted, err
}

// Scan calls fn for every document in _id order until fn returns false.
func (db *DB) Scan(coll string, fn func(d map[string]any) bool) error {
	if err := validCollection(coll); err != nil {
		return err
	}
	prefix := docPrefix(coll)
	return db.p.View(func(t *btree.Tree) error {
		var cur []byte // id prefix of the document being assembled
		var buf []byte
		flush := func() (bool, error) {
			if cur == nil {
				return true, nil
			}
			d, err := Decode(buf)
			if err != nil {
				return false, err
			}
			return fn(d), nil
		}
		for it := t.Seek(prefix); it.Valid() && bytes.HasPrefix(it.Key(), prefix); it.Next() {
			key := it.Key()
			idEnd := len(key) - 2 // drop the chunk number; the byte before it is \x00
			if cur == nil || !bytes.Equal(key[:idEnd], cur) {
				if more, err := flush(); err != nil || !more {
					return err
				}
				cur, buf = bytes.Clone(key[:idEnd]), nil
			}
			buf = append(buf, it.Value()...)
		}
		_, err := flush()
		return err
	})
}

// Count returns the number of documents in a collection.
func (db *DB) Count(coll string) (int64, error) {
	if err := validCollection(coll); err != nil {
		return 0, err
	}
	var n int64
	err := db.p.View(func(t *btree.Tree) error {
		var err error
		n, _, err = readCount(t, coll)
		return err
	})
	return n, err
}

// Collections lists collection names in order.
func (db *DB) Collections() ([]string, error) {
	var names []string
	err := db.p.View(func(t *btree.Tree) error {
		for it := t.Seek([]byte("c/")); it.Valid() && bytes.HasPrefix(it.Key(), []byte("c/")); it.Next() {
			names = append(names, string(it.Key()[2:]))
		}
		return nil
	})
	return names, err
}

// DropCollection deletes a collection and all its documents, and reports
// whether it existed.
func (db *DB) DropCollection(coll string) (bool, error) {
	if err := validCollection(coll); err != nil {
		return false, err
	}
	var existed bool
	err := db.p.Update(func(t *btree.Tree) error {
		_, existed, _ = readCount(t, coll)
		// Collect first: deleting while iterating would invalidate the iterator.
		var keys [][]byte
		prefix := docPrefix(coll)
		for it := t.Seek(prefix); it.Valid() && bytes.HasPrefix(it.Key(), prefix); it.Next() {
			keys = append(keys, bytes.Clone(it.Key()))
		}
		for _, k := range keys {
			if _, err := t.Delete(k); err != nil {
				return err
			}
		}
		_, err := t.Delete(metaKey(coll))
		return err
	})
	return existed, err
}

// idGen makes MongoDB-style ObjectIDs: 4 bytes of Unix time, 5 random bytes
// fixed per process, and a 3-byte counter, as 24 hex characters. IDs from one
// process sort in creation order, except when the counter (which starts at a
// random value, as in MongoDB) wraps within the same second.
type idGen struct {
	mu      sync.Mutex
	random  [5]byte
	counter uint32
	now     func() time.Time
}

func newIDGen() *idGen {
	g := &idGen{now: time.Now}
	rand.Read(g.random[:])
	var c [4]byte
	rand.Read(c[:])
	g.counter = binary.BigEndian.Uint32(c[:]) & 0xffffff
	return g
}

func (g *idGen) next() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var b [12]byte
	binary.BigEndian.PutUint32(b[0:], uint32(g.now().Unix()))
	copy(b[4:9], g.random[:])
	g.counter = (g.counter + 1) & 0xffffff
	b[9], b[10], b[11] = byte(g.counter>>16), byte(g.counter>>8), byte(g.counter)
	return hex.EncodeToString(b[:])
}
