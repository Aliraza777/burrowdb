package btree

import (
	"bytes"
	"errors"
	"fmt"
)

// Iter walks keys in ascending order.
type Iter struct {
	t    *Tree
	path []node   // root to leaf
	pos  []uint16 // index within each node on the path
}

// Seek returns an iterator at the first key >= key. Seek(nil) starts at the
// smallest key.
func (t *Tree) Seek(key []byte) *Iter {
	it := &Iter{t: t}
	if t.Root == 0 {
		return it
	}
	for ptr := t.Root; ; {
		n := t.get(ptr)
		idx := lookupLE(n, key)
		it.path = append(it.path, n)
		it.pos = append(it.pos, idx)
		if n.btype() == nodeLeaf {
			break
		}
		ptr = n.getPtr(idx)
	}
	// We are at the last key <= key; skip it if it's smaller, or the sentinel.
	for it.Valid() && (len(it.Key()) == 0 || bytes.Compare(it.Key(), key) < 0) {
		it.Next()
	}
	return it
}

// Valid reports whether the iterator is at a key.
func (it *Iter) Valid() bool {
	last := len(it.path) - 1
	return last >= 0 && it.pos[last] < it.path[last].nkeys()
}

// Key returns the current key. It is only valid until the tree changes.
func (it *Iter) Key() []byte {
	last := len(it.path) - 1
	return it.path[last].getKey(it.pos[last])
}

// Value returns the current value. It is only valid until the tree changes.
func (it *Iter) Value() []byte {
	last := len(it.path) - 1
	return it.path[last].getVal(it.pos[last])
}

// Next moves to the next key.
func (it *Iter) Next() {
	level := len(it.path) - 1
	for level >= 0 {
		it.pos[level]++
		if it.pos[level] < it.path[level].nkeys() {
			break
		}
		level--
	}
	if level < 0 {
		it.path, it.pos = nil, nil
		return
	}
	// Descend to the leftmost leaf of the new subtree.
	for l := level + 1; l < len(it.path); l++ {
		it.path[l] = it.t.get(it.path[l-1].getPtr(it.pos[l-1]))
		it.pos[l] = 0
	}
}

// Verify checks the tree's structure: keys strictly ascending, each internal
// key equal to its child's first key, every node within PageSize, and all
// leaves at the same depth. It also reports every reachable page to visit.
func (t *Tree) Verify(visit func(ptr uint64)) error {
	if t.Root == 0 {
		return nil
	}
	leafDepth := -1
	var walk func(ptr uint64, depth int, lower []byte) error
	walk = func(ptr uint64, depth int, lower []byte) error {
		if visit != nil {
			visit(ptr)
		}
		n := t.get(ptr)
		if n.nbytes() > PageSize {
			return fmt.Errorf("page %d: %d bytes", ptr, n.nbytes())
		}
		nk := n.nkeys()
		if nk == 0 {
			return fmt.Errorf("page %d: empty node", ptr)
		}
		if lower != nil && !bytes.Equal(n.getKey(0), lower) {
			return fmt.Errorf("page %d: first key %q, parent says %q", ptr, n.getKey(0), lower)
		}
		for i := uint16(1); i < nk; i++ {
			if bytes.Compare(n.getKey(i-1), n.getKey(i)) >= 0 {
				return fmt.Errorf("page %d: keys out of order at %d", ptr, i)
			}
		}
		switch n.btype() {
		case nodeLeaf:
			if leafDepth == -1 {
				leafDepth = depth
			} else if depth != leafDepth {
				return fmt.Errorf("page %d: leaf at depth %d, others at %d", ptr, depth, leafDepth)
			}
			return nil
		case nodeInternal:
			for i := range nk {
				if err := walk(n.getPtr(i), depth+1, n.getKey(i)); err != nil {
					return err
				}
			}
			return nil
		default:
			return errors.New("unknown node type")
		}
	}
	return walk(t.Root, 0, nil)
}
