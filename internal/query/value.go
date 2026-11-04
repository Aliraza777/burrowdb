// Package query evaluates MongoDB-style queries over documents: filters
// ($eq, $gt, $in, $and, ...), sort, skip, limit and projection.
package query

import (
	"slices"
	"strconv"
	"strings"
)

// lookup returns every value at a dotted path. As in MongoDB, an array met
// along the way is searched element by element ("items.price" finds the price
// of each item), and a numeric part can also index into it ("items.0.price").
func lookup(v any, parts []string) []any {
	if len(parts) == 0 {
		return []any{v}
	}
	switch x := v.(type) {
	case map[string]any:
		child, ok := x[parts[0]]
		if !ok {
			return nil
		}
		return lookup(child, parts[1:])
	case []any:
		var out []any
		if i, err := strconv.Atoi(parts[0]); err == nil && i >= 0 && i < len(x) {
			out = append(out, lookup(x[i], parts[1:])...)
		}
		for _, el := range x {
			if m, ok := el.(map[string]any); ok {
				out = append(out, lookup(m, parts)...)
			}
		}
		return out
	default:
		return nil
	}
}

func splitPath(path string) []string { return strings.Split(path, ".") }

// matchCandidates is what a field condition is tested against: each value at
// the path, plus the elements of any array value, so {"tags": "a"} matches
// tags: ["a", "b"] and {"tags": ["a", "b"]} matches the array itself.
func matchCandidates(values []any) []any {
	out := slices.Clone(values)
	for _, v := range values {
		if arr, ok := v.([]any); ok {
			out = append(out, arr...)
		}
	}
	return out
}

func asNumber(v any) (float64, int64, bool, bool) {
	switch n := v.(type) {
	case int64:
		return float64(n), n, true, true
	case int:
		return float64(n), int64(n), true, true
	case float64:
		return n, 0, false, true
	}
	return 0, 0, false, false
}

// compareNumbers compares two numbers exactly when both are integers, and as
// float64 otherwise (so 1 == 1.0, as in MongoDB).
func compareNumbers(a, b any) int {
	af, ai, aInt, _ := asNumber(a)
	bf, bi, bInt, _ := asNumber(b)
	if aInt && bInt {
		return cmpOrdered(ai, bi)
	}
	// NaN equals only NaN and sorts below every other number, as in MongoDB.
	if aNaN, bNaN := af != af, bf != bf; aNaN || bNaN {
		switch {
		case aNaN && bNaN:
			return 0
		case aNaN:
			return -1
		}
		return 1
	}
	return cmpOrdered(af, bf)
}

func cmpOrdered[T int64 | float64 | string](a, b T) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// typeRank orders values of different types, following MongoDB's order for
// the types documents can hold: null < numbers < strings < objects < arrays < booleans.
func typeRank(v any) int {
	switch v.(type) {
	case nil:
		return 0
	case int64, int, float64:
		return 1
	case string:
		return 2
	case map[string]any:
		return 3
	case []any:
		return 4
	case bool:
		return 5
	}
	return 6
}

// compareTotal orders any two values: by type rank, then by value.
func compareTotal(a, b any) int {
	ra, rb := typeRank(a), typeRank(b)
	if ra != rb {
		return cmpOrdered(int64(ra), int64(rb))
	}
	switch x := a.(type) {
	case nil:
		return 0
	case int64, int, float64:
		return compareNumbers(a, b)
	case string:
		return cmpOrdered(x, b.(string))
	case bool:
		y := b.(bool)
		switch {
		case x == y:
			return 0
		case !x:
			return -1
		}
		return 1
	case []any:
		y := b.([]any)
		for i := range min(len(x), len(y)) {
			if c := compareTotal(x[i], y[i]); c != 0 {
				return c
			}
		}
		return cmpOrdered(int64(len(x)), int64(len(y)))
	case map[string]any:
		y := b.(map[string]any)
		xk, yk := sortedKeys(x), sortedKeys(y)
		for i := range min(len(xk), len(yk)) {
			if c := cmpOrdered(xk[i], yk[i]); c != 0 {
				return c
			}
			if c := compareTotal(x[xk[i]], y[yk[i]]); c != 0 {
				return c
			}
		}
		return cmpOrdered(int64(len(xk)), int64(len(yk)))
	}
	return 0
}

func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

func equal(a, b any) bool {
	return typeRank(a) == typeRank(b) && compareTotal(a, b) == 0
}

// comparable reports whether a range operator can compare a and b: only
// within the same type (numbers count as one type), as in MongoDB.
func comparable(a, b any) bool {
	ra := typeRank(a)
	return ra == typeRank(b) && ra != 3 && ra != 4
}
