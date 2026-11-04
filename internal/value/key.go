package value

import (
	"encoding/binary"
	"math"
)

// Index keys encode scalar values so that comparing the bytes gives the same
// order as Compare. Each starts with a tag in MongoDB's type order:
const (
	keyNull   byte = 0x10
	keyNumber byte = 0x20
	keyString byte = 0x30
	keyBool   byte = 0x60
)

// Key returns the order-preserving encoding of a scalar value (null, number,
// string or bool). Objects and arrays aren't indexed, so ok is false for them.
//
//	null    0x10
//	number  0x20 + 8 bytes: the float64 with its sign flipped for positives and
//	        all bits flipped for negatives, so bytes sort numerically. -0 is
//	        stored as 0 and NaN as all zeros (below every number). Integers
//	        beyond 2^53 can share a key, so index ranges must be inclusive and
//	        results re-checked against the filter.
//	string  0x30 + bytes with each 0x00 written as 0x00 0xFF, then 0x00 0x01
//	bool    0x60 + 0 or 1
//
// No key is a prefix of another, so a key followed by a document id can be
// split apart again (see KeyLen).
func Key(v any) ([]byte, bool) {
	switch x := v.(type) {
	case nil:
		return []byte{keyNull}, true
	case int64, int, float64:
		f, _, _, _ := asNumber(v)
		return binary.BigEndian.AppendUint64([]byte{keyNumber}, sortableFloat(f)), true
	case string:
		b := make([]byte, 0, len(x)+3)
		b = append(b, keyString)
		for i := 0; i < len(x); i++ {
			if x[i] == 0 {
				b = append(b, 0, 0xFF)
			} else {
				b = append(b, x[i])
			}
		}
		return append(b, 0, 1), true
	case bool:
		if x {
			return []byte{keyBool, 1}, true
		}
		return []byte{keyBool, 0}, true
	}
	return nil, false
}

func sortableFloat(f float64) uint64 {
	if f != f { // NaN
		return 0
	}
	if f == 0 {
		f = 0 // turns -0 into +0
	}
	bits := math.Float64bits(f)
	if bits>>63 == 1 {
		return ^bits
	}
	return bits | 1<<63
}

// KeyLen returns the length of the key at the start of b, or -1 if b doesn't
// start with a complete key.
func KeyLen(b []byte) int {
	if len(b) == 0 {
		return -1
	}
	switch b[0] {
	case keyNull:
		return 1
	case keyNumber:
		if len(b) < 9 {
			return -1
		}
		return 9
	case keyBool:
		if len(b) < 2 {
			return -1
		}
		return 2
	case keyString:
		for i := 1; i+1 < len(b); i++ {
			if b[i] == 0 {
				if b[i+1] == 1 {
					return i + 2
				}
				i++ // an escaped zero byte
			}
		}
	}
	return -1
}

// TypeStart and TypeEnd bound all keys of v's type: every key k of that type
// has TypeStart(v) <= k < TypeEnd(v).
func TypeStart(v any) []byte {
	k, _ := Key(v)
	return k[:1]
}

func TypeEnd(v any) []byte {
	k, _ := Key(v)
	return []byte{k[0] + 1}
}

// After returns the smallest byte string greater than every index entry
// that starts with key k (k followed by a document id of printable ASCII).
func After(k []byte) []byte {
	return append(append([]byte(nil), k...), 0xFF)
}
