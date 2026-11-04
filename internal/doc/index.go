package doc

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Aliraza777/burrowdb/internal/btree"
	"github.com/Aliraza777/burrowdb/internal/value"
)

// Secondary indexes live in the same B+tree:
//
//	i/<collection>\x00<field>                     index metadata (multikey flag)
//	x/<collection>\x00<field>\x00<value key><_id>  one entry per indexed value
//
// <value key> is value.Key, an order-preserving encoding, so the entries for
// one field sort by value and a range of values is a range of keys. Following
// MongoDB, an array is indexed once per element ("multikey") and a missing
// field is indexed as null. Objects and arrays themselves aren't indexed;
// queries on them don't use an index.

var (
	ErrIndexExists   = errors.New("doc: index already exists")
	ErrIndexNotFound = errors.New("doc: index not found")
	ErrBadField      = errors.New("doc: invalid field path")
)

// IndexInfo describes one index.
type IndexInfo struct {
	Field string
	// Multikey is set once any document has several values for the field. A
	// query planner may then not intersect range conditions on it, because
	// each condition can be met by a different element.
	Multikey bool
}

func validField(field string) error {
	if field == "" || field == "_id" || strings.HasPrefix(field, "$") || strings.Contains(field, "\x00") {
		return ErrBadField
	}
	for _, part := range strings.Split(field, ".") {
		if part == "" {
			return ErrBadField
		}
	}
	return nil
}

func indexMetaPrefix(coll string) []byte { return []byte("i/" + coll + "\x00") }
func indexMetaKey(coll, field string) []byte {
	return append(indexMetaPrefix(coll), field...)
}
func entriesPrefix(coll string) []byte { return []byte("x/" + coll + "\x00") }
func entryPrefix(coll, field string) []byte {
	return append(entriesPrefix(coll), field+"\x00"...)
}

// entryValueKeys returns the distinct value keys a document has for a field,
// and whether it has more than one value there (which makes the index multikey).
func entryValueKeys(d map[string]any, field string) ([][]byte, bool) {
	values := value.Lookup(d, value.SplitPath(field))
	if len(values) == 0 {
		k, _ := value.Key(nil)
		return [][]byte{k}, false
	}
	var keys [][]byte
	for _, c := range value.Candidates(values) {
		k, ok := value.Key(c)
		if !ok {
			continue
		}
		if !slices.ContainsFunc(keys, func(e []byte) bool { return bytes.Equal(e, k) }) {
			keys = append(keys, k)
		}
	}
	multikey := len(values) > 1
	for _, v := range values {
		if _, isArr := v.([]any); isArr {
			multikey = true
		}
	}
	return keys, multikey
}

func readIndexes(t *btree.Tree, coll string) ([]IndexInfo, error) {
	prefix := indexMetaPrefix(coll)
	var out []IndexInfo
	for it := t.Seek(prefix); it.Valid() && bytes.HasPrefix(it.Key(), prefix); it.Next() {
		m, err := Decode(it.Value())
		if err != nil {
			return nil, err
		}
		mk, _ := m["multikey"].(bool)
		out = append(out, IndexInfo{Field: string(it.Key()[len(prefix):]), Multikey: mk})
	}
	return out, nil
}

func writeIndexMeta(t *btree.Tree, coll string, info IndexInfo) error {
	enc, _ := Encode(map[string]any{"multikey": info.Multikey})
	return t.Set(indexMetaKey(coll, info.Field), enc)
}

// indexDoc adds (or with remove, deletes) a document's entries in every index.
func indexDoc(t *btree.Tree, coll, id string, d map[string]any, indexes []IndexInfo, remove bool) error {
	for i, idx := range indexes {
		keys, multikey := entryValueKeys(d, idx.Field)
		for _, k := range keys {
			key := append(append(entryPrefix(coll, idx.Field), k...), id...)
			if remove {
				if _, err := t.Delete(key); err != nil {
					return err
				}
			} else if err := t.Set(key, nil); err != nil {
				return err
			}
		}
		if !remove && multikey && !idx.Multikey {
			indexes[i].Multikey = true
			if err := writeIndexMeta(t, coll, indexes[i]); err != nil {
				return err
			}
		}
	}
	return nil
}

// deletePrefix removes every key that starts with prefix.
func deletePrefix(t *btree.Tree, prefix []byte) error {
	// Collect first: deleting while iterating would invalidate the iterator.
	var keys [][]byte
	for it := t.Seek(prefix); it.Valid() && bytes.HasPrefix(it.Key(), prefix); it.Next() {
		keys = append(keys, bytes.Clone(it.Key()))
	}
	for _, k := range keys {
		if _, err := t.Delete(k); err != nil {
			return err
		}
	}
	return nil
}

// CreateIndex indexes a field of a collection, including its existing documents.
func (db *DB) CreateIndex(coll, field string) error {
	if err := validCollection(coll); err != nil {
		return err
	}
	if err := validField(field); err != nil {
		return err
	}
	return db.p.Update(func(t *btree.Tree) error {
		if _, exists := t.Get(indexMetaKey(coll, field)); exists {
			return ErrIndexExists
		}
		// Read every document first; inserting while scanning would invalidate the scan.
		type pending struct {
			id string
			d  map[string]any
		}
		var docs []pending
		err := scanTree(t, coll, func(d map[string]any) bool {
			docs = append(docs, pending{d["_id"].(string), d})
			return true
		})
		if err != nil {
			return err
		}
		info := []IndexInfo{{Field: field}}
		if err := writeIndexMeta(t, coll, info[0]); err != nil {
			return err
		}
		for _, p := range docs {
			if err := indexDoc(t, coll, p.id, p.d, info, false); err != nil {
				return err
			}
		}
		// Make sure the collection exists, so an index can be created up front.
		if _, ok := t.Get(metaKey(coll)); !ok {
			return addCount(t, coll, 0)
		}
		return nil
	})
}

// DropIndex removes an index.
func (db *DB) DropIndex(coll, field string) error {
	if err := validCollection(coll); err != nil {
		return err
	}
	return db.p.Update(func(t *btree.Tree) error {
		if _, exists := t.Get(indexMetaKey(coll, field)); !exists {
			return ErrIndexNotFound
		}
		if err := deletePrefix(t, entryPrefix(coll, field)); err != nil {
			return err
		}
		_, err := t.Delete(indexMetaKey(coll, field))
		return err
	})
}

// Indexes lists a collection's indexes.
func (db *DB) Indexes(coll string) ([]IndexInfo, error) {
	var out []IndexInfo
	err := db.Read(func(r *Reader) error {
		var err error
		out, err = r.Indexes(coll)
		return err
	})
	return out, err
}

// Reader reads from one consistent snapshot of the database.
type Reader struct{ t *btree.Tree }

// Read runs fn with a Reader. Writers wait until it returns.
func (db *DB) Read(fn func(r *Reader) error) error {
	return db.p.View(func(t *btree.Tree) error { return fn(&Reader{t}) })
}

// Get returns a document, or ErrNotFound.
func (r *Reader) Get(coll, id string) (map[string]any, error) {
	if err := validCollection(coll); err != nil {
		return nil, err
	}
	enc, _, found := readDoc(r.t, coll, id)
	if !found {
		return nil, ErrNotFound
	}
	return Decode(enc)
}

// Scan calls fn for every document in _id order until fn returns false.
func (r *Reader) Scan(coll string, fn func(d map[string]any) bool) error {
	if err := validCollection(coll); err != nil {
		return err
	}
	return scanTree(r.t, coll, fn)
}

// Count returns the number of documents in a collection.
func (r *Reader) Count(coll string) (int64, error) {
	n, _, err := readCount(r.t, coll)
	return n, err
}

// Indexes lists a collection's indexes.
func (r *Reader) Indexes(coll string) ([]IndexInfo, error) {
	if err := validCollection(coll); err != nil {
		return nil, err
	}
	return readIndexes(r.t, coll)
}

// ScanIndex calls fn with the _id of every entry of an index whose value key
// is in [lo, hi), in key order, until fn returns false. A document can appear
// more than once in a multikey index.
func (r *Reader) ScanIndex(coll, field string, lo, hi []byte, fn func(id string) bool) error {
	prefix := entryPrefix(coll, field)
	start := append(bytes.Clone(prefix), lo...)
	end := append(bytes.Clone(prefix), hi...)
	for it := r.t.Seek(start); it.Valid() && bytes.Compare(it.Key(), end) < 0; it.Next() {
		rest := it.Key()[len(prefix):]
		n := value.KeyLen(rest)
		if n < 0 {
			return fmt.Errorf("doc: corrupt index entry in %s.%s", coll, field)
		}
		if !fn(string(rest[n:])) {
			return nil
		}
	}
	return nil
}

func scanTree(t *btree.Tree, coll string, fn func(d map[string]any) bool) error {
	prefix := docPrefix(coll)
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
}

// CheckIndexes verifies that every index of every collection holds exactly
// the entries its documents should produce, and that multikey flags are set
// where needed.
func (db *DB) CheckIndexes() error {
	return db.Read(func(r *Reader) error {
		colls, err := collections(r.t)
		if err != nil {
			return err
		}
		for _, coll := range colls {
			indexes, err := readIndexes(r.t, coll)
			if err != nil {
				return err
			}
			for _, idx := range indexes {
				want := map[string]bool{}
				needMultikey := false
				err := scanTree(r.t, coll, func(d map[string]any) bool {
					keys, mk := entryValueKeys(d, idx.Field)
					needMultikey = needMultikey || mk
					for _, k := range keys {
						want[string(k)+d["_id"].(string)] = true
					}
					return true
				})
				if err != nil {
					return err
				}
				prefix := entryPrefix(coll, idx.Field)
				got := 0
				for it := r.t.Seek(prefix); it.Valid() && bytes.HasPrefix(it.Key(), prefix); it.Next() {
					entry := string(it.Key()[len(prefix):])
					if !want[entry] {
						return fmt.Errorf("index %s.%s has a stray entry %q", coll, idx.Field, entry)
					}
					got++
				}
				if got != len(want) {
					return fmt.Errorf("index %s.%s has %d entries, documents need %d", coll, idx.Field, got, len(want))
				}
				if needMultikey && !idx.Multikey {
					return fmt.Errorf("index %s.%s should be marked multikey", coll, idx.Field)
				}
			}
		}
		return nil
	})
}
