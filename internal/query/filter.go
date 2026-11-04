package query

import (
	"fmt"
	"strings"
)

// Filter is a parsed MongoDB-style filter.
type Filter struct{ root node }

type node interface {
	match(d map[string]any) bool
}

type andNode []node
type orNode []node
type norNode []node

func (n andNode) match(d map[string]any) bool {
	for _, c := range n {
		if !c.match(d) {
			return false
		}
	}
	return true
}

func (n orNode) match(d map[string]any) bool {
	for _, c := range n {
		if c.match(d) {
			return true
		}
	}
	return false
}

func (n norNode) match(d map[string]any) bool { return !orNode(n).match(d) }

type fieldNode struct {
	path []string
	ops  []fieldOp // all must hold
}

type fieldOp struct {
	name string // $eq, $ne, $gt, $gte, $lt, $lte, $in, $nin, $exists
	val  any
	list []any
}

func (f fieldNode) match(d map[string]any) bool {
	values := lookup(d, f.path)
	cands := matchCandidates(values)
	for _, op := range f.ops {
		if !op.holds(values, cands) {
			return false
		}
	}
	return true
}

func eqAny(values, cands []any, v any) bool {
	// {"x": null} also matches documents without x, as in MongoDB.
	if v == nil && len(values) == 0 {
		return true
	}
	for _, c := range cands {
		if equal(c, v) {
			return true
		}
	}
	return false
}

func (op fieldOp) holds(values, cands []any) bool {
	switch op.name {
	case "$eq":
		return eqAny(values, cands, op.val)
	case "$ne":
		return !eqAny(values, cands, op.val)
	case "$in", "$nin":
		in := false
		for _, v := range op.list {
			if eqAny(values, cands, v) {
				in = true
				break
			}
		}
		return in == (op.name == "$in")
	case "$exists":
		return (len(values) > 0) == op.val.(bool)
	}
	// Range operators: any candidate of the same type in range.
	for _, c := range cands {
		if !comparable(c, op.val) {
			continue
		}
		cmp := compareTotal(c, op.val)
		switch op.name {
		case "$gt":
			if cmp > 0 {
				return true
			}
		case "$gte":
			if cmp >= 0 {
				return true
			}
		case "$lt":
			if cmp < 0 {
				return true
			}
		case "$lte":
			if cmp <= 0 {
				return true
			}
		}
	}
	return false
}

// Match reports whether a document satisfies the filter.
func (f *Filter) Match(d map[string]any) bool {
	return f == nil || f.root == nil || f.root.match(d)
}

// ParseFilter turns a filter document into a Filter. An empty or nil filter
// matches everything.
func ParseFilter(m map[string]any) (*Filter, error) {
	root, err := parseAnd(m, 0)
	if err != nil {
		return nil, err
	}
	return &Filter{root: root}, nil
}

const maxFilterDepth = 50

func parseAnd(m map[string]any, depth int) (node, error) {
	if depth > maxFilterDepth {
		return nil, fmt.Errorf("query: filter nested deeper than %d levels", maxFilterDepth)
	}
	var parts andNode
	for _, k := range sortedKeys(m) { // deterministic order, for error messages
		v := m[k]
		switch {
		case k == "$and" || k == "$or" || k == "$nor":
			arr, ok := v.([]any)
			if !ok || len(arr) == 0 {
				return nil, fmt.Errorf("query: %s needs a non-empty array of filters", k)
			}
			var subs []node
			for i, item := range arr {
				sub, ok := item.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("query: %s[%d] must be a filter object", k, i)
				}
				n, err := parseAnd(sub, depth+1)
				if err != nil {
					return nil, err
				}
				subs = append(subs, n)
			}
			switch k {
			case "$and":
				parts = append(parts, andNode(subs))
			case "$or":
				parts = append(parts, orNode(subs))
			default:
				parts = append(parts, norNode(subs))
			}
		case strings.HasPrefix(k, "$"):
			return nil, fmt.Errorf("query: unknown top-level operator %s", k)
		default:
			if k == "" || strings.Contains(k, "..") || strings.HasPrefix(k, ".") || strings.HasSuffix(k, ".") {
				return nil, fmt.Errorf("query: invalid field path %q", k)
			}
			ops, err := parseField(k, v)
			if err != nil {
				return nil, err
			}
			parts = append(parts, fieldNode{path: splitPath(k), ops: ops})
		}
	}
	return parts, nil
}

// parseField reads a field's condition: an operator object like
// {"$gt": 1, "$lt": 5}, or a plain value meaning equality.
func parseField(path string, v any) ([]fieldOp, error) {
	obj, isObj := v.(map[string]any)
	if !isObj || len(obj) == 0 {
		return []fieldOp{{name: "$eq", val: v}}, nil
	}
	dollar := 0
	for k := range obj {
		if strings.HasPrefix(k, "$") {
			dollar++
		}
	}
	if dollar == 0 {
		return []fieldOp{{name: "$eq", val: v}}, nil // equality with an object
	}
	if dollar != len(obj) {
		return nil, fmt.Errorf("query: %s mixes operators and plain fields", path)
	}
	var ops []fieldOp
	for _, name := range sortedKeys(obj) {
		arg := obj[name]
		switch name {
		case "$eq", "$ne", "$gt", "$gte", "$lt", "$lte":
			ops = append(ops, fieldOp{name: name, val: arg})
		case "$in", "$nin":
			list, ok := arg.([]any)
			if !ok {
				return nil, fmt.Errorf("query: %s.%s needs an array", path, name)
			}
			ops = append(ops, fieldOp{name: name, list: list})
		case "$exists":
			b, ok := arg.(bool)
			if !ok {
				return nil, fmt.Errorf("query: %s.$exists needs true or false", path)
			}
			ops = append(ops, fieldOp{name: name, val: b})
		default:
			return nil, fmt.Errorf("query: unknown operator %s on %s", name, path)
		}
	}
	return ops, nil
}
