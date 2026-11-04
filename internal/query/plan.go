package query

import (
	"bytes"
	"fmt"
	"slices"
	"strings"

	"github.com/Aliraza777/burrowdb/internal/doc"
	"github.com/Aliraza777/burrowdb/internal/value"
)

// keyRange is a half-open range [lo, hi) of index value keys.
type keyRange struct{ lo, hi []byte }

// candidate is one way to run a query with an index.
type candidate struct {
	field    string
	ranges   []keyRange
	bounds   string // human-readable, for explain
	estimate int    // index entries in the ranges, counted up to a cap
}

// Explain describes how a query ran.
type Explain struct {
	Plan         string   `json:"plan"` // "IXSCAN" or "COLLSCAN"
	Index        string   `json:"index,omitempty"`
	Bounds       string   `json:"bounds,omitempty"`
	Considered   []string `json:"considered,omitempty"` // indexes weighed, with entry counts
	KeysExamined int      `json:"keysExamined"`
	DocsExamined int      `json:"docsExamined"`
	Returned     int      `json:"returned"`
}

// conjuncts returns the field conditions that must all hold, looking inside
// top-level $and.
func conjuncts(n node) []fieldNode {
	switch x := n.(type) {
	case andNode:
		var out []fieldNode
		for _, c := range x {
			out = append(out, conjuncts(c)...)
		}
		return out
	case fieldNode:
		return []fieldNode{x}
	}
	return nil
}

// rangesFor turns one field's conditions into index key ranges. ok is false
// when no condition can use an index. The ranges may cover more than the
// conditions match (they are inclusive at the edges, and multikey fields
// don't intersect), so results are always re-checked against the filter.
func rangesFor(ops []fieldOp, multikey bool) ([]keyRange, string, bool) {
	for _, op := range ops {
		if op.name != "$eq" {
			continue
		}
		if k, ok := value.Key(op.val); ok {
			return []keyRange{{k, value.After(k)}}, fmt.Sprintf("[%s]", show(op.val)), true
		}
	}
	for _, op := range ops {
		if op.name != "$in" {
			continue
		}
		var keys [][]byte
		for _, v := range op.list {
			k, ok := value.Key(v)
			if !ok {
				keys = nil
				break
			}
			if !slices.ContainsFunc(keys, func(e []byte) bool { return bytes.Equal(e, k) }) {
				keys = append(keys, k)
			}
		}
		if keys == nil {
			continue
		}
		slices.SortFunc(keys, bytes.Compare)
		var rs []keyRange
		for _, k := range keys {
			rs = append(rs, keyRange{k, value.After(k)})
		}
		return rs, fmt.Sprintf("%d points %s", len(rs), show(op.list)), true
	}

	var cur keyRange
	var desc []string
	used := 0
	for _, op := range ops {
		k, ok := value.Key(op.val)
		if !ok {
			continue
		}
		// $gt and $lt can skip the key itself, unless it is an integer beyond
		// 2^53, whose key it may share with neighbouring integers.
		exact := !hugeInt(op.val)
		var r keyRange
		switch op.name {
		case "$gt":
			if exact {
				r = keyRange{value.After(k), value.TypeEnd(op.val)}
			} else {
				r = keyRange{k, value.TypeEnd(op.val)}
			}
		case "$gte":
			r = keyRange{k, value.TypeEnd(op.val)}
		case "$lt":
			if exact {
				r = keyRange{value.TypeStart(op.val), k}
			} else {
				r = keyRange{value.TypeStart(op.val), value.After(k)}
			}
		case "$lte":
			r = keyRange{value.TypeStart(op.val), value.After(k)}
		default:
			continue
		}
		switch {
		case used == 0:
			cur = r
		case multikey:
			// Each condition may be met by a different array element, so the
			// conditions can't be intersected; keep the first one only.
			continue
		default:
			if bytes.Compare(r.lo, cur.lo) > 0 {
				cur.lo = r.lo
			}
			if bytes.Compare(r.hi, cur.hi) < 0 {
				cur.hi = r.hi
			}
		}
		used++
		desc = append(desc, op.name+" "+show(op.val))
	}
	if used == 0 {
		return nil, "", false
	}
	if bytes.Compare(cur.lo, cur.hi) >= 0 {
		return nil, strings.Join(desc, ", ") + " (empty)", true
	}
	return []keyRange{cur}, strings.Join(desc, ", "), true
}

func show(v any) string {
	out, err := doc.ToJSON(map[string]any{"v": v})
	if err != nil {
		return fmt.Sprint(v)
	}
	return strings.TrimSuffix(strings.TrimPrefix(string(out), `{"v":`), "}")
}

// countUpTo counts index entries in the ranges, stopping at limit.
func countUpTo(r *doc.Reader, coll, field string, ranges []keyRange, limit int) (int, error) {
	n := 0
	for _, kr := range ranges {
		err := r.ScanIndex(coll, field, kr.lo, kr.hi, func(string) bool {
			n++
			return n < limit
		})
		if err != nil || n >= limit {
			return n, err
		}
	}
	return n, nil
}

// choose picks an index for the filter, or nil for a collection scan. hint
// "$natural" forces a scan; a field name forces that field's index.
func choose(r *doc.Reader, coll string, f *Filter, hint string, ex *Explain) (*candidate, error) {
	if hint == "$natural" || f == nil || f.root == nil {
		return nil, nil
	}
	indexes, err := r.Indexes(coll)
	if err != nil {
		return nil, err
	}
	multikey := map[string]bool{}
	for _, idx := range indexes {
		multikey[idx.Field] = idx.Multikey
	}
	var cands []*candidate
	for _, fn := range conjuncts(f.root) {
		field := strings.Join(fn.path, ".")
		mk, indexed := multikey[field]
		if !indexed || (hint != "" && hint != field) {
			continue
		}
		ranges, bounds, ok := rangesFor(fn.ops, mk)
		if ok && !slices.ContainsFunc(cands, func(c *candidate) bool { return c.field == field }) {
			cands = append(cands, &candidate{field: field, ranges: ranges, bounds: bounds})
		}
	}
	if hint != "" {
		if len(cands) == 0 {
			return nil, fmt.Errorf("query: hint %q: no usable index on that field for this filter", hint)
		}
		return cands[0], nil
	}
	if len(cands) == 0 {
		return nil, nil
	}

	total, err := r.Count(coll)
	if err != nil {
		return nil, err
	}
	// Fetching through an index jumps around the file, so an index that would
	// read half the collection or more loses to a straight scan.
	limit := max(1, int(total/2))
	var best *candidate
	for _, c := range cands {
		n, err := countUpTo(r, coll, c.field, c.ranges, limit)
		if err != nil {
			return nil, err
		}
		c.estimate = n
		capped := ""
		if n >= limit {
			capped = "+"
		}
		ex.Considered = append(ex.Considered, fmt.Sprintf("%s: %d%s entries", c.field, n, capped))
		if n < limit && (best == nil || n < best.estimate) {
			best = c
			limit = n + 1 // later candidates only need to beat this one
		}
	}
	return best, nil
}

func hugeInt(v any) bool {
	n, ok := v.(int64)
	return ok && (n > 1<<53 || n < -(1<<53))
}
