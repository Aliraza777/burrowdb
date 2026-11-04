// Package btree is a copy-on-write B+tree over fixed-size pages.
//
// The tree never modifies a page in place: an update writes new copies of the
// pages on the path from the root to the changed leaf, and frees the old
// ones. Whoever stores the pages (see package pager) can therefore make a
// change durable by writing the new pages first and switching the root last.
package btree

import (
	"bytes"
	"errors"
)

// Pages stores the tree's nodes.
type Pages interface {
	// Get returns the page; the caller must not modify it.
	Get(ptr uint64) []byte
	// New stores a PageSize-long page and returns its pointer (never 0).
	New(page []byte) uint64
	// Del frees a page that is no longer part of the tree.
	Del(ptr uint64)
}

var (
	ErrEmptyKey      = errors.New("btree: empty key")
	ErrKeyTooLarge   = errors.New("btree: key too large")
	ErrValueTooLarge = errors.New("btree: value too large")
)

// Tree is a B+tree whose root page is Root (0 for an empty tree).
//
// Every tree has an empty "sentinel" key as the first key of its leftmost
// leaf, so a lookup always has a key <= the one searched for. That is why
// user keys can't be empty.
type Tree struct {
	Root  uint64
	Pages Pages
}

func checkKV(key, val []byte) error {
	switch {
	case len(key) == 0:
		return ErrEmptyKey
	case len(key) > MaxKeySize:
		return ErrKeyTooLarge
	case len(val) > MaxValueSize:
		return ErrValueTooLarge
	}
	return nil
}

func (t *Tree) get(ptr uint64) node { return node(t.Pages.Get(ptr)) }

// Get returns a copy of the value stored under key.
func (t *Tree) Get(key []byte) ([]byte, bool) {
	if t.Root == 0 || len(key) == 0 {
		return nil, false
	}
	n := t.get(t.Root)
	for {
		idx := lookupLE(n, key)
		if n.btype() == nodeLeaf {
			if bytes.Equal(n.getKey(idx), key) {
				return bytes.Clone(n.getVal(idx)), true
			}
			return nil, false
		}
		n = t.get(n.getPtr(idx))
	}
}

// Set inserts key or replaces its value.
func (t *Tree) Set(key, val []byte) error {
	if err := checkKV(key, val); err != nil {
		return err
	}
	if t.Root == 0 {
		root := make(node, PageSize)
		root.setHeader(nodeLeaf, 2)
		appendKV(root, 0, 0, nil, nil) // the sentinel
		appendKV(root, 1, 0, key, val)
		t.Root = t.Pages.New(root)
		return nil
	}
	old := t.get(t.Root)
	t.Pages.Del(t.Root)
	n := t.insert(old, key, val)
	count, parts := split3(n)
	if count == 1 {
		t.Root = t.Pages.New(parts[0])
		return nil
	}
	// The root split: add a level.
	root := make(node, PageSize)
	root.setHeader(nodeInternal, count)
	for i, kid := range parts[:count] {
		appendKV(root, uint16(i), t.Pages.New(kid), kid.getKey(0), nil)
	}
	t.Root = t.Pages.New(root)
	return nil
}

// insert returns a copy of n with key set; the result may be up to 2 pages
// long and is split by the caller.
func (t *Tree) insert(n node, key, val []byte) node {
	out := make(node, 2*PageSize)
	idx := lookupLE(n, key)
	if n.btype() == nodeLeaf {
		if bytes.Equal(key, n.getKey(idx)) {
			leafUpdate(out, n, idx, key, val)
		} else {
			leafInsert(out, n, idx+1, key, val)
		}
		return out
	}
	kptr := n.getPtr(idx)
	kid := t.insert(t.get(kptr), key, val)
	t.Pages.Del(kptr)
	count, parts := split3(kid)
	t.replaceKids(out, n, idx, parts[:count]...)
	return out
}

// replaceKids writes n's entries into out with child idx replaced by kids.
func (t *Tree) replaceKids(out, n node, idx uint16, kids ...node) {
	inc := uint16(len(kids))
	out.setHeader(nodeInternal, n.nkeys()+inc-1)
	appendRange(out, n, 0, 0, idx)
	for i, kid := range kids {
		appendKV(out, idx+uint16(i), t.Pages.New(kid), kid.getKey(0), nil)
	}
	appendRange(out, n, idx+inc, idx+1, n.nkeys()-idx-1)
}

// Delete removes key and reports whether it existed.
func (t *Tree) Delete(key []byte) (bool, error) {
	if err := checkKV(key, nil); err != nil {
		return false, err
	}
	if t.Root == 0 {
		return false, nil
	}
	updated := t.delete(t.get(t.Root), key)
	if updated == nil {
		return false, nil
	}
	t.Pages.Del(t.Root)
	if updated.btype() == nodeInternal && updated.nkeys() == 1 {
		// The root has one child left: remove a level.
		t.Root = updated.getPtr(0)
	} else {
		t.Root = t.Pages.New(updated)
	}
	return true, nil
}

// delete returns a copy of n without key, or nil if key isn't there.
func (t *Tree) delete(n node, key []byte) node {
	idx := lookupLE(n, key)
	if n.btype() == nodeLeaf {
		if !bytes.Equal(key, n.getKey(idx)) {
			return nil
		}
		out := make(node, PageSize)
		leafDelete(out, n, idx)
		return out
	}

	kptr := n.getPtr(idx)
	updated := t.delete(t.get(kptr), key)
	if updated == nil {
		return nil
	}
	t.Pages.Del(kptr)

	out := make(node, PageSize)
	dir, sibling := t.shouldMerge(n, idx, updated)
	switch {
	case dir < 0:
		merged := make(node, PageSize)
		nodeMerge(merged, sibling, updated)
		t.Pages.Del(n.getPtr(idx - 1))
		replace2Kids(out, n, idx-1, t.Pages.New(merged), merged.getKey(0))
	case dir > 0:
		merged := make(node, PageSize)
		nodeMerge(merged, updated, sibling)
		t.Pages.Del(n.getPtr(idx + 1))
		replace2Kids(out, n, idx, t.Pages.New(merged), merged.getKey(0))
	case updated.nkeys() == 0:
		// An only child became empty. Pass an empty node up; the parent merges
		// it away, since only the leftmost path (which holds the sentinel) can
		// have no left sibling.
		out.setHeader(nodeInternal, 0)
	default:
		t.replaceKids(out, n, idx, updated)
	}
	return out
}

// shouldMerge decides whether a shrunken child should merge with a sibling:
// -1 for the left one, +1 for the right one, 0 for neither.
func (t *Tree) shouldMerge(n node, idx uint16, updated node) (int, node) {
	if updated.nbytes() > PageSize/4 {
		return 0, nil
	}
	if idx > 0 {
		sibling := t.get(n.getPtr(idx - 1))
		if sibling.nbytes()+updated.nbytes()-header <= PageSize {
			return -1, sibling
		}
	}
	if idx+1 < n.nkeys() {
		sibling := t.get(n.getPtr(idx + 1))
		if sibling.nbytes()+updated.nbytes()-header <= PageSize {
			return 1, sibling
		}
	}
	return 0, nil
}

// replace2Kids writes n's entries into out with children idx and idx+1
// replaced by one child.
func replace2Kids(out, n node, idx uint16, ptr uint64, key []byte) {
	out.setHeader(nodeInternal, n.nkeys()-1)
	appendRange(out, n, 0, 0, idx)
	appendKV(out, idx, ptr, key, nil)
	appendRange(out, n, idx+1, idx+2, n.nkeys()-idx-2)
}
