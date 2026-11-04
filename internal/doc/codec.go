package doc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
)

// Values inside a document are one of: nil, bool, int64, float64, string,
// []any, or map[string]any. Integers and floats are kept apart, as in MongoDB.
//
// Binary encoding, one tag byte per value:
//
//	0 null   1 false   2 true
//	3 int64     zig-zag varint
//	4 float64   8 bytes, IEEE 754 little-endian
//	5 string    uvarint length, bytes
//	6 array     uvarint count, values
//	7 object    uvarint count, then (uvarint key length, key, value) sorted by key
const (
	tagNull byte = iota
	tagFalse
	tagTrue
	tagInt
	tagFloat
	tagString
	tagArray
	tagObject
)

// MaxDepth bounds nesting, so a hostile document can't exhaust the stack.
const MaxDepth = 100

var ErrCorruptDocument = errors.New("doc: corrupt encoded document")

// Encode returns the binary form of a document.
func Encode(d map[string]any) ([]byte, error) {
	return appendValue(nil, d, 0)
}

func appendValue(b []byte, v any, depth int) ([]byte, error) {
	if depth > MaxDepth {
		return nil, fmt.Errorf("doc: nested deeper than %d levels", MaxDepth)
	}
	switch v := v.(type) {
	case nil:
		return append(b, tagNull), nil
	case bool:
		if v {
			return append(b, tagTrue), nil
		}
		return append(b, tagFalse), nil
	case int64:
		return binary.AppendVarint(append(b, tagInt), v), nil
	case int:
		return binary.AppendVarint(append(b, tagInt), int64(v)), nil
	case float64:
		return binary.LittleEndian.AppendUint64(append(b, tagFloat), math.Float64bits(v)), nil
	case string:
		b = binary.AppendUvarint(append(b, tagString), uint64(len(v)))
		return append(b, v...), nil
	case []any:
		b = binary.AppendUvarint(append(b, tagArray), uint64(len(v)))
		var err error
		for _, item := range v {
			if b, err = appendValue(b, item, depth+1); err != nil {
				return nil, err
			}
		}
		return b, nil
	case map[string]any:
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		b = binary.AppendUvarint(append(b, tagObject), uint64(len(v)))
		var err error
		for _, k := range keys {
			b = binary.AppendUvarint(b, uint64(len(k)))
			b = append(b, k...)
			if b, err = appendValue(b, v[k], depth+1); err != nil {
				return nil, err
			}
		}
		return b, nil
	default:
		return nil, fmt.Errorf("doc: unsupported value type %T", v)
	}
}

// Decode parses a document produced by Encode. Corrupt input returns
// ErrCorruptDocument; it never panics.
func Decode(b []byte) (map[string]any, error) {
	d := decoder{b: b}
	v := d.value(0)
	if d.err != nil {
		return nil, d.err
	}
	if d.pos != len(b) {
		return nil, fmt.Errorf("%w: %d trailing bytes", ErrCorruptDocument, len(b)-d.pos)
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: top level is not an object", ErrCorruptDocument)
	}
	return m, nil
}

type decoder struct {
	b   []byte
	pos int
	err error
}

func (d *decoder) fail(what string) {
	if d.err == nil {
		d.err = fmt.Errorf("%w: %s at byte %d", ErrCorruptDocument, what, d.pos)
	}
}

func (d *decoder) byte() byte {
	if d.err != nil || d.pos >= len(d.b) {
		d.fail("unexpected end")
		return 0
	}
	c := d.b[d.pos]
	d.pos++
	return c
}

// count reads a length and rejects any longer than the remaining input,
// since every element takes at least one byte.
func (d *decoder) count() int {
	if d.err != nil {
		return 0
	}
	n, size := binary.Uvarint(d.b[d.pos:])
	// A length must be in its shortest form, so each document has one encoding.
	if size <= 0 || size != len(binary.AppendUvarint(nil, n)) || n > uint64(len(d.b)-d.pos-size) {
		d.fail("bad length")
		return 0
	}
	d.pos += size
	return int(n)
}

func (d *decoder) bytes(n int) []byte {
	if d.err != nil {
		return nil
	}
	s := d.b[d.pos : d.pos+n] // count() already checked n against the input
	d.pos += n
	return s
}

func (d *decoder) value(depth int) any {
	if depth > MaxDepth {
		d.fail("too deeply nested")
		return nil
	}
	switch tag := d.byte(); tag {
	case tagNull:
		return nil
	case tagFalse:
		return false
	case tagTrue:
		return true
	case tagInt:
		if d.err != nil {
			return nil
		}
		v, size := binary.Varint(d.b[d.pos:])
		if size <= 0 || size != len(binary.AppendVarint(nil, v)) {
			d.fail("bad or non-minimal integer")
			return nil
		}
		d.pos += size
		return v
	case tagFloat:
		if d.err != nil || len(d.b)-d.pos < 8 {
			d.fail("short float")
			return nil
		}
		v := math.Float64frombits(binary.LittleEndian.Uint64(d.b[d.pos:]))
		d.pos += 8
		return v
	case tagString:
		return string(d.bytes(d.count()))
	case tagArray:
		n := d.count()
		arr := make([]any, 0, n)
		for i := 0; i < n && d.err == nil; i++ {
			arr = append(arr, d.value(depth+1))
		}
		return arr
	case tagObject:
		n := d.count()
		obj := make(map[string]any, n)
		prev := ""
		for i := 0; i < n && d.err == nil; i++ {
			k := string(d.bytes(d.count()))
			if i > 0 && k <= prev {
				d.fail("object keys not sorted or duplicated")
				return nil
			}
			prev = k
			obj[k] = d.value(depth + 1)
		}
		return obj
	default:
		d.fail(fmt.Sprintf("unknown tag %d", tag))
		return nil
	}
}
