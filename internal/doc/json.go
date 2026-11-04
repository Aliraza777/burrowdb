package doc

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
)

// FromJSON parses a JSON object into a document. Whole numbers that fit in
// an int64 become int64; other numbers become float64.
func FromJSON(data []byte) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("doc: invalid JSON: %w", err)
	}
	if dec.More() {
		return nil, errors.New("doc: invalid JSON: more than one value")
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("doc: a document must be a JSON object")
	}
	out, err := convertNumbers(m, 0)
	if err != nil {
		return nil, err
	}
	return out.(map[string]any), nil
}

func convertNumbers(v any, depth int) (any, error) {
	if depth > MaxDepth {
		return nil, fmt.Errorf("doc: nested deeper than %d levels", MaxDepth)
	}
	switch v := v.(type) {
	case json.Number:
		if !strings.ContainsAny(string(v), ".eE") {
			if i, err := v.Int64(); err == nil {
				return i, nil
			}
		}
		f, err := v.Float64()
		if err != nil || math.IsInf(f, 0) {
			return nil, fmt.Errorf("doc: number %s out of range", v)
		}
		return f, nil
	case []any:
		for i, item := range v {
			conv, err := convertNumbers(item, depth+1)
			if err != nil {
				return nil, err
			}
			v[i] = conv
		}
		return v, nil
	case map[string]any:
		for k, item := range v {
			conv, err := convertNumbers(item, depth+1)
			if err != nil {
				return nil, err
			}
			v[k] = conv
		}
		return v, nil
	default:
		return v, nil
	}
}

// ToJSON renders a document as JSON with keys in sorted order and _id first.
func ToJSON(d map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	first := true
	write := func(k string, v any) error {
		if !first {
			buf.WriteByte(',')
		}
		first = false
		kb, _ := json.Marshal(k)
		buf.Write(kb)
		buf.WriteByte(':')
		vb, err := json.Marshal(v) // map keys are sorted by encoding/json
		if err != nil {
			return err
		}
		buf.Write(vb)
		return nil
	}
	if id, ok := d["_id"]; ok {
		if err := write("_id", id); err != nil {
			return nil, err
		}
	}
	keys := make([]string, 0, len(d))
	for k := range d {
		if k != "_id" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	for _, k := range keys {
		if err := write(k, d[k]); err != nil {
			return nil, err
		}
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}
