package value

import (
	"bytes"
	"math"
	"math/rand/v2"
	"testing"
)

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	}
	return 0
}

var keySamples = []any{
	nil, false, true,
	int64(0), int64(1), int64(-1), int64(42), int64(math.MaxInt64), int64(math.MinInt64),
	0.0, math.Copysign(0, -1), 0.5, -0.5, 1.0, 41.999, 42.0, 1e300, -1e300,
	math.Inf(1), math.Inf(-1), math.NaN(), math.SmallestNonzeroFloat64, -math.SmallestNonzeroFloat64,
	"", "a", "A", "ab", "a\x00", "a\x00b", "a\x01", "\x00", "\xff", "z", "héllo",
}

// Byte order of keys must equal Compare's order, for every pair.
func TestKeyOrderMatchesCompare(t *testing.T) {
	for _, a := range keySamples {
		for _, b := range keySamples {
			ka, _ := Key(a)
			kb, _ := Key(b)
			want := sign(Compare(a, b))
			// Integers beyond 2^53 may share a key; that's allowed (never reversed).
			if got := sign(bytes.Compare(ka, kb)); got != want && got != 0 {
				t.Errorf("Key(%#v) vs Key(%#v): bytes say %d, Compare says %d", a, b, got, want)
			} else if got == 0 && want != 0 && !bigInts(a, b) {
				t.Errorf("Key(%#v) == Key(%#v) but Compare says %d", a, b, want)
			}
		}
	}
}

func bigInts(a, b any) bool {
	ai, aok := a.(int64)
	bi, bok := b.(int64)
	return aok && bok && (ai > 1<<53 || ai < -(1<<53) || bi > 1<<53 || bi < -(1<<53))
}

func TestKeyLenAndNoPrefixes(t *testing.T) {
	for _, a := range keySamples {
		ka, _ := Key(a)
		if n := KeyLen(append(bytes.Clone(ka), "someid"...)); n != len(ka) {
			t.Errorf("KeyLen(Key(%#v)+id) = %d, want %d", a, n, len(ka))
		}
		for _, b := range keySamples {
			kb, _ := Key(b)
			if len(ka) < len(kb) && bytes.HasPrefix(kb, ka) {
				t.Errorf("Key(%#v) is a prefix of Key(%#v)", a, b)
			}
		}
	}
	for _, v := range []any{[]any{1}, map[string]any{}} {
		if _, ok := Key(v); ok {
			t.Errorf("Key(%v) should not be indexable", v)
		}
	}
	for _, bad := range [][]byte{nil, {keyNumber, 1}, {keyString, 'a'}, {keyString, 'a', 0}, {keyBool}, {0x99}} {
		if KeyLen(bad) != -1 {
			t.Errorf("KeyLen(%x) accepted an incomplete key", bad)
		}
	}
}

func TestTypeBounds(t *testing.T) {
	for _, a := range keySamples {
		k, _ := Key(a)
		if bytes.Compare(k, TypeStart(a)) < 0 || bytes.Compare(k, TypeEnd(a)) >= 0 {
			t.Errorf("Key(%#v) outside its type bounds", a)
		}
		if bytes.Compare(After(k), k) <= 0 {
			t.Errorf("After(Key(%#v)) not greater", a)
		}
	}
}

// Random numbers and strings: byte order must match Compare.
func FuzzKeyOrder(f *testing.F) {
	f.Add(uint64(0), uint64(1), "a", "b", byte(0))
	f.Fuzz(func(t *testing.T, x, y uint64, s1, s2 string, mode byte) {
		var a, b any
		switch mode % 3 {
		case 0:
			a, b = math.Float64frombits(x), math.Float64frombits(y)
		case 1:
			a, b = int64(x>>11)-int64(1<<52), math.Float64frombits(y) // ints within 2^53 vs floats
		default:
			a, b = s1, s2
		}
		ka, _ := Key(a)
		kb, _ := Key(b)
		if got, want := sign(bytes.Compare(ka, kb)), sign(Compare(a, b)); got != want {
			t.Fatalf("%#v vs %#v: bytes %d, Compare %d", a, b, got, want)
		}
	})
}

func TestRandomIntsWithinPrecision(t *testing.T) {
	r := rand.New(rand.NewPCG(1, 2))
	for range 100_000 {
		a := r.Int64N(1<<54) - 1<<53
		b := r.Int64N(1<<54) - 1<<53
		ka, _ := Key(a)
		kb, _ := Key(b)
		if sign(bytes.Compare(ka, kb)) != sign(Compare(a, b)) {
			t.Fatalf("%d vs %d", a, b)
		}
	}
}
