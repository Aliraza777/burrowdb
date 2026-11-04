package query

import (
	"errors"
	"fmt"
	"strings"

	"github.com/Aliraza777/burrowdb/internal/value"
)

// Projection picks the fields a query returns. It either includes the listed
// fields or excludes them; _id is included unless it's set to 0.
type Projection struct {
	include bool
	paths   [][]string
	id      bool
}

// ParseProjection reads {"name": 1, "address.city": 1} (include) or
// {"password": 0} (exclude). Values may be 0/1 or true/false.
// An empty projection returns nil, which keeps documents whole.
func ParseProjection(m map[string]any) (*Projection, error) {
	if len(m) == 0 {
		return nil, nil
	}
	p := &Projection{id: true}
	mode := 0 // 1 include, -1 exclude
	for _, path := range value.SortedKeys(m) {
		on, err := projectionFlag(path, m[path])
		if err != nil {
			return nil, err
		}
		if path == "_id" {
			p.id = on
			continue
		}
		if path == "" || strings.HasPrefix(path, "$") || strings.Contains(path, "..") {
			return nil, fmt.Errorf("query: invalid projection field %q", path)
		}
		want := -1
		if on {
			want = 1
		}
		if mode != 0 && mode != want {
			return nil, errors.New("query: a projection can't mix including and excluding fields (except _id)")
		}
		mode = want
		p.paths = append(p.paths, value.SplitPath(path))
	}
	// Only {"_id": 0} or {"_id": 1}: exclude _id, or keep just _id.
	p.include = mode == 1 || (mode == 0 && p.id)
	return p, nil
}

func projectionFlag(path string, v any) (bool, error) {
	switch x := v.(type) {
	case bool:
		return x, nil
	case int64:
		if x == 0 || x == 1 {
			return x == 1, nil
		}
	case float64:
		if x == 0 || x == 1 {
			return x == 1, nil
		}
	}
	return false, fmt.Errorf("query: projection value for %q must be 0, 1, true or false", path)
}

// Apply returns a new document with the projection applied.
func (p *Projection) Apply(d map[string]any) map[string]any {
	if p == nil {
		return d
	}
	if p.include {
		out := map[string]any{}
		for _, path := range p.paths {
			copyPath(d, out, path)
		}
		if id, ok := d["_id"]; ok && p.id {
			out["_id"] = id
		}
		return out
	}
	out := deepCopy(d).(map[string]any)
	for _, path := range p.paths {
		deletePath(out, path)
	}
	if !p.id {
		delete(out, "_id")
	}
	return out
}

func copyPath(src, dst map[string]any, path []string) {
	v, ok := src[path[0]]
	if !ok {
		return
	}
	if len(path) == 1 {
		dst[path[0]] = deepCopy(v)
		return
	}
	sub, ok := v.(map[string]any)
	if !ok {
		return
	}
	child, ok := dst[path[0]].(map[string]any)
	if !ok {
		child = map[string]any{}
		dst[path[0]] = child
	}
	copyPath(sub, child, path[1:])
}

func deletePath(m map[string]any, path []string) {
	if len(path) == 1 {
		delete(m, path[0])
		return
	}
	if sub, ok := m[path[0]].(map[string]any); ok {
		deletePath(sub, path[1:])
	}
}

func deepCopy(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[k] = deepCopy(item)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = deepCopy(item)
		}
		return out
	}
	return v
}
