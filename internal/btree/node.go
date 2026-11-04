package btree

import (
	"bytes"
	"encoding/binary"
)

// PageSize is the size of every node on disk.
const PageSize = 4096

// Limits that guarantee any single key-value pair fits in one node.
const (
	MaxKeySize   = 1000
	MaxValueSize = 3000
)

const (
	nodeInternal uint16 = 1
	nodeLeaf     uint16 = 2
)

// A node is one page, laid out as:
//
//	| type (2) | nkeys (2) | pointers (nkeys × 8) | offsets (nkeys × 2) | key-values |
//
// Each key-value is | klen (2) | vlen (2) | key | value |. Offsets give the end
// of each key-value relative to the start of the key-value area; the start of
// the first one is implicitly 0. Internal nodes store child pointers and the
// first key of each child, with empty values. Leaves store keys and values,
// with zero pointers.
const header = 4

type node []byte

func (n node) btype() uint16 { return binary.LittleEndian.Uint16(n[0:2]) }
func (n node) nkeys() uint16 { return binary.LittleEndian.Uint16(n[2:4]) }

func (n node) setHeader(btype, nkeys uint16) {
	binary.LittleEndian.PutUint16(n[0:2], btype)
	binary.LittleEndian.PutUint16(n[2:4], nkeys)
}

func (n node) getPtr(i uint16) uint64 {
	return binary.LittleEndian.Uint64(n[header+8*i:])
}

func (n node) setPtr(i uint16, ptr uint64) {
	binary.LittleEndian.PutUint64(n[header+8*i:], ptr)
}

func (n node) offsetPos(i uint16) uint16 { return header + 8*n.nkeys() + 2*(i-1) }

func (n node) getOffset(i uint16) uint16 {
	if i == 0 {
		return 0
	}
	return binary.LittleEndian.Uint16(n[n.offsetPos(i):])
}

func (n node) setOffset(i, off uint16) {
	binary.LittleEndian.PutUint16(n[n.offsetPos(i):], off)
}

func (n node) kvPos(i uint16) uint16 {
	return header + 10*n.nkeys() + n.getOffset(i)
}

func (n node) getKey(i uint16) []byte {
	pos := n.kvPos(i)
	klen := binary.LittleEndian.Uint16(n[pos:])
	return n[pos+4:][:klen]
}

func (n node) getVal(i uint16) []byte {
	pos := n.kvPos(i)
	klen := binary.LittleEndian.Uint16(n[pos:])
	vlen := binary.LittleEndian.Uint16(n[pos+2:])
	return n[pos+4+klen:][:vlen]
}

// nbytes is the used size of the node.
func (n node) nbytes() uint16 { return n.kvPos(n.nkeys()) }

// lookupLE returns the index of the last key <= key. Key 0 of every node is
// <= any key in its subtree, so a result always exists.
func lookupLE(n node, key []byte) uint16 {
	found := uint16(0)
	lo, hi := uint16(1), n.nkeys()
	for lo < hi {
		mid := lo + (hi-lo)/2
		if bytes.Compare(n.getKey(mid), key) <= 0 {
			found = mid
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return found
}

// appendKV writes entry idx. Entries must be written in order, after setHeader.
func appendKV(n node, idx uint16, ptr uint64, key, val []byte) {
	n.setPtr(idx, ptr)
	pos := n.kvPos(idx)
	binary.LittleEndian.PutUint16(n[pos:], uint16(len(key)))
	binary.LittleEndian.PutUint16(n[pos+2:], uint16(len(val)))
	copy(n[pos+4:], key)
	copy(n[pos+4+uint16(len(key)):], val)
	n.setOffset(idx+1, n.getOffset(idx)+4+uint16(len(key)+len(val)))
}

// appendRange copies count entries from old[src:] into n[dst:].
func appendRange(n, old node, dst, src, count uint16) {
	for i := range count {
		appendKV(n, dst+i, old.getPtr(src+i), old.getKey(src+i), old.getVal(src+i))
	}
}

func leafInsert(n, old node, idx uint16, key, val []byte) {
	n.setHeader(nodeLeaf, old.nkeys()+1)
	appendRange(n, old, 0, 0, idx)
	appendKV(n, idx, 0, key, val)
	appendRange(n, old, idx+1, idx, old.nkeys()-idx)
}

func leafUpdate(n, old node, idx uint16, key, val []byte) {
	n.setHeader(nodeLeaf, old.nkeys())
	appendRange(n, old, 0, 0, idx)
	appendKV(n, idx, 0, key, val)
	appendRange(n, old, idx+1, idx+1, old.nkeys()-idx-1)
}

func leafDelete(n, old node, idx uint16) {
	n.setHeader(nodeLeaf, old.nkeys()-1)
	appendRange(n, old, 0, 0, idx)
	appendRange(n, old, idx, idx+1, old.nkeys()-idx-1)
}

func nodeMerge(n, left, right node) {
	n.setHeader(left.btype(), left.nkeys()+right.nkeys())
	appendRange(n, left, 0, 0, left.nkeys())
	appendRange(n, right, left.nkeys(), 0, right.nkeys())
}

// split2 splits old so that right fits in a page; left may still be too big.
func split2(left, right, old node) {
	nkeys := old.nkeys()
	nleft := nkeys / 2
	leftBytes := func() uint16 { return header + 10*nleft + old.getOffset(nleft) }
	for leftBytes() > PageSize {
		nleft--
	}
	rightBytes := func() uint16 { return old.nbytes() - leftBytes() + header }
	for rightBytes() > PageSize {
		nleft++
	}
	nright := nkeys - nleft
	left.setHeader(old.btype(), nleft)
	right.setHeader(old.btype(), nright)
	appendRange(left, old, 0, 0, nleft)
	appendRange(right, old, 0, nleft, nright)
}

// split3 splits an oversized node into 1, 2 or 3 page-sized nodes.
func split3(old node) (uint16, [3]node) {
	if old.nbytes() <= PageSize {
		return 1, [3]node{old[:PageSize]}
	}
	left := make(node, 2*PageSize)
	right := make(node, PageSize)
	split2(left, right, old)
	if left.nbytes() <= PageSize {
		return 2, [3]node{left[:PageSize], right}
	}
	leftleft := make(node, PageSize)
	middle := make(node, PageSize)
	split2(leftleft, middle, left)
	return 3, [3]node{leftleft, middle, right}
}
