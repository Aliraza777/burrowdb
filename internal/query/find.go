package query

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/Aliraza777/burrowdb/internal/doc"
	"github.com/Aliraza777/burrowdb/internal/value"
)

// SortField sorts by Path, ascending if Desc is false.
type SortField struct {
	Path string
	Desc bool
}

// Query is everything a find takes. A zero Query returns every document.
type Query struct {
	Filter     *Filter
	Sort       []SortField
	Skip       int
	Limit      int // 0 means no limit
	Projection *Projection
	// Hint forces a plan: "$natural" for a collection scan, or a field name
	// to use that field's index. Empty lets the planner choose.
	Hint string
}

// sortValue is the value a document sorts by for one field: missing counts as
// null, and for an array the smallest element is used ascending, the largest
// descending (an empty array counts as null).
func sortValue(d map[string]any, f SortField) any {
	values := value.Lookup(d, value.SplitPath(f.Path))
	var best any
	found := false
	for _, v := range value.Candidates(values) {
		if _, isArr := v.([]any); isArr {
			continue
		}
		if !found {
			best, found = v, true
			continue
		}
		c := value.Compare(v, best)
		if (!f.Desc && c < 0) || (f.Desc && c > 0) {
			best = v
		}
	}
	return best
}

func sortDocs(docs []map[string]any, fields []SortField) {
	if len(fields) == 0 {
		return
	}
	// Stable, so documents that tie keep _id order.
	slices.SortStableFunc(docs, func(a, b map[string]any) int {
		for _, f := range fields {
			c := value.Compare(sortValue(a, f), sortValue(b, f))
			if f.Desc {
				c = -c
			}
			if c != 0 {
				return c
			}
		}
		return 0
	})
}

// Find runs a query. It uses an index when one helps (see Explain).
func Find(db *doc.DB, coll string, q Query) ([]map[string]any, error) {
	out, _, err := run(db.Read, coll, q)
	return out, err
}

// FindIn runs a query against a Reader, such as tx.Reader() inside a
// transaction, so it sees that transaction's uncommitted writes.
func FindIn(r *doc.Reader, coll string, q Query) ([]map[string]any, error) {
	out, _, err := run(func(fn func(*doc.Reader) error) error { return fn(r) }, coll, q)
	return out, err
}

// ExplainQuery runs a query and reports the plan it used and the work done.
func ExplainQuery(db *doc.DB, coll string, q Query) (Explain, error) {
	_, ex, err := run(db.Read, coll, q)
	return ex, err
}

// Count returns how many documents in a collection match the filter.
func Count(db *doc.DB, coll string, f *Filter) (int, error) {
	out, _, err := run(db.Read, coll, Query{Filter: f})
	return len(out), err
}

func run(read func(func(*doc.Reader) error) error, coll string, q Query) ([]map[string]any, Explain, error) {
	var ex Explain
	if q.Skip < 0 || q.Limit < 0 {
		return nil, ex, errors.New("query: skip and limit can't be negative")
	}
	// Without a sort, results come in _id order, so reading can stop as soon
	// as skip+limit documents matched.
	stopAt := -1
	if len(q.Sort) == 0 && q.Limit > 0 {
		stopAt = q.Skip + q.Limit
	}
	var out []map[string]any
	err := read(func(r *doc.Reader) error {
		plan, err := choose(r, coll, q.Filter, q.Hint, &ex)
		if err != nil {
			return err
		}
		if plan == nil {
			ex.Plan = "COLLSCAN"
			return r.Scan(coll, func(d map[string]any) bool {
				ex.DocsExamined++
				if q.Filter.Match(d) {
					out = append(out, d)
				}
				return stopAt < 0 || len(out) < stopAt
			})
		}
		ex.Plan, ex.Index, ex.Bounds = "IXSCAN", plan.field, plan.bounds
		// Collect the matching _ids (a multikey index can repeat one), then
		// fetch in _id order so results come out as a scan would return them.
		seen := map[string]bool{}
		var ids []string
		for _, kr := range plan.ranges {
			err := r.ScanIndex(coll, plan.field, kr.lo, kr.hi, func(id string) bool {
				ex.KeysExamined++
				if !seen[id] {
					seen[id] = true
					ids = append(ids, id)
				}
				return true
			})
			if err != nil {
				return err
			}
		}
		slices.Sort(ids)
		for _, id := range ids {
			if stopAt >= 0 && len(out) >= stopAt {
				break
			}
			d, err := r.Get(coll, id)
			if err != nil {
				return fmt.Errorf("query: index %s points at %s: %w", plan.field, id, err)
			}
			ex.DocsExamined++
			if q.Filter.Match(d) { // the index narrows; the filter decides
				out = append(out, d)
			}
		}
		return nil
	})
	if err != nil {
		return nil, ex, err
	}
	sortDocs(out, q.Sort)
	out = out[min(q.Skip, len(out)):]
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	if q.Projection != nil {
		for i, d := range out {
			out[i] = q.Projection.Apply(d)
		}
	}
	ex.Returned = len(out)
	return out, ex, nil
}

// ParseQueryJSON reads a query of the form
//
//	{"filter": {...}, "sort": {"age": -1, "name": 1}, "skip": 0, "limit": 10, "projection": {"name": 1}}
//
// Every part is optional. The sort's field order is kept from the JSON text.
func ParseQueryJSON(data []byte) (Query, error) {
	var raw struct {
		Filter     json.RawMessage `json:"filter"`
		Sort       json.RawMessage `json:"sort"`
		Skip       int             `json:"skip"`
		Limit      int             `json:"limit"`
		Projection json.RawMessage `json:"projection"`
		Hint       string          `json:"hint"`
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return Query{}, fmt.Errorf("query: %w", err)
	}
	if raw.Skip < 0 || raw.Limit < 0 {
		return Query{}, errors.New("query: skip and limit can't be negative")
	}
	var q Query
	q.Skip, q.Limit, q.Hint = raw.Skip, raw.Limit, raw.Hint
	if len(raw.Filter) > 0 && string(raw.Filter) != "null" {
		m, err := doc.FromJSON(raw.Filter)
		if err != nil {
			return Query{}, fmt.Errorf("query: filter: %w", err)
		}
		if q.Filter, err = ParseFilter(m); err != nil {
			return Query{}, err
		}
	}
	if len(raw.Sort) > 0 && string(raw.Sort) != "null" {
		var err error
		if q.Sort, err = parseSortJSON(raw.Sort); err != nil {
			return Query{}, err
		}
	}
	if len(raw.Projection) > 0 && string(raw.Projection) != "null" {
		m, err := doc.FromJSON(raw.Projection)
		if err != nil {
			return Query{}, fmt.Errorf("query: projection: %w", err)
		}
		if q.Projection, err = ParseProjection(m); err != nil {
			return Query{}, err
		}
	}
	return q, nil
}

// parseSortJSON reads {"field": 1 | -1, ...} keeping the fields' order, which
// a Go map would lose.
func parseSortJSON(data []byte) ([]SortField, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("query: sort must be an object like {\"age\": -1}")
	}
	var out []SortField
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("query: sort: %w", err)
		}
		path := t.(string) // object keys are always strings
		v, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("query: sort: %w", err)
		}
		num, ok := v.(json.Number)
		if !ok || (num != "1" && num != "-1") {
			return nil, fmt.Errorf("query: sort direction for %q must be 1 or -1", path)
		}
		if path == "" || strings.HasPrefix(path, "$") {
			return nil, fmt.Errorf("query: invalid sort field %q", path)
		}
		out = append(out, SortField{Path: path, Desc: num == "-1"})
	}
	return out, nil
}
