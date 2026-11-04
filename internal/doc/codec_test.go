package doc

import (
	"bytes"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	docs := []map[string]any{
		{},
		{"a": nil, "b": true, "c": false},
		{"int": int64(42), "neg": int64(-7), "float": 42.0, "pi": math.Pi},
		{"big": int64(math.MaxInt64), "small": int64(math.MinInt64), "tiny": math.SmallestNonzeroFloat64},
		{"s": "", "u": "héllo 世界 🦫", "zero": "a\x00b"},
		{"arr": []any{int64(1), "two", 3.5, nil, []any{}, map[string]any{"x": true}}},
		{"nested": map[string]any{"a": map[string]any{"b": map[string]any{"c": int64(1)}}}},
	}
	for _, d := range docs {
		enc, err := Encode(d)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Decode(enc)
		if err != nil {
			t.Fatalf("Decode(%v): %v", d, err)
		}
		if !reflect.DeepEqual(got, d) {
			t.Fatalf("round trip changed %#v into %#v", d, got)
		}
	}
}

func TestIntAndFloatStayDistinct(t *testing.T) {
	enc, _ := Encode(map[string]any{"i": int64(42), "f": 42.0})
	d, _ := Decode(enc)
	if _, ok := d["i"].(int64); !ok {
		t.Fatalf("i decoded as %T", d["i"])
	}
	if _, ok := d["f"].(float64); !ok {
		t.Fatalf("f decoded as %T", d["f"])
	}
}

func TestEncodingIsCanonical(t *testing.T) {
	a, _ := Encode(map[string]any{"z": int64(1), "a": int64(2), "m": int64(3)})
	b, _ := Encode(map[string]any{"m": int64(3), "z": int64(1), "a": int64(2)})
	if !bytes.Equal(a, b) {
		t.Fatal("the same document encoded two ways")
	}
}

func TestRejectsUnsupportedAndTooDeep(t *testing.T) {
	if _, err := Encode(map[string]any{"x": struct{}{}}); err == nil {
		t.Fatal("encoded an unsupported type")
	}
	var deep any = int64(1)
	for range MaxDepth + 2 {
		deep = []any{deep}
	}
	if _, err := Encode(map[string]any{"d": deep}); err == nil {
		t.Fatal("encoded a document nested too deeply")
	}
}

func TestDecodeRejectsCorruptInput(t *testing.T) {
	good, _ := Encode(map[string]any{"a": "hello", "b": []any{int64(1), 2.5}})
	cases := map[string][]byte{
		"empty":         {},
		"truncated":     good[:len(good)-3],
		"trailing":      append(bytes.Clone(good), 0),
		"unknown tag":   {9},
		"not an object": {tagTrue},
		"huge length":   {tagString, 0xff, 0xff, 0xff, 0xff, 0x0f},
		"unsorted keys": {tagObject, 2, 1, 'b', tagNull, 1, 'a', tagNull},
		"duplicate key": {tagObject, 2, 1, 'a', tagNull, 1, 'a', tagNull},
	}
	for name, b := range cases {
		if _, err := Decode(b); !errors.Is(err, ErrCorruptDocument) {
			t.Errorf("%s: got %v", name, err)
		}
	}
}

// Decode must never panic, and whatever it accepts must re-encode to the
// exact same bytes (the format has one encoding per document).
func FuzzDecode(f *testing.F) {
	seed, _ := Encode(map[string]any{"a": []any{int64(1), "x", 2.5, nil, true}, "b": map[string]any{"c": false}})
	f.Add(seed)
	f.Add([]byte{tagObject, 0})
	f.Fuzz(func(t *testing.T, b []byte) {
		d, err := Decode(b)
		if err != nil {
			return
		}
		again, err := Encode(d)
		if err != nil {
			t.Fatalf("decoded document doesn't re-encode: %v", err)
		}
		if !bytes.Equal(again, b) {
			t.Fatalf("not canonical:\n in  %x\n out %x", b, again)
		}
	})
}

func TestFromJSON(t *testing.T) {
	d, err := FromJSON([]byte(`{"n": 3, "f": 3.0, "e": 1e3, "big": 12345678901234567890, "s": "x", "a": [1, 2.5], "o": {"k": null}}`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"n": int64(3), "f": 3.0, "e": 1000.0, "big": 12345678901234567890.0, "s": "x",
		"a": []any{int64(1), 2.5}, "o": map[string]any{"k": nil},
	}
	if !reflect.DeepEqual(d, want) {
		t.Fatalf("FromJSON = %#v", d)
	}
	for _, bad := range []string{`[1,2]`, `"x"`, `{"a":1} {"b":2}`, `{"a":`, `{"x": 1e999}`} {
		if _, err := FromJSON([]byte(bad)); err == nil {
			t.Errorf("FromJSON(%s) accepted", bad)
		}
	}
	deep := strings.Repeat(`{"a":`, MaxDepth+2) + "1" + strings.Repeat("}", MaxDepth+2)
	if _, err := FromJSON([]byte(deep)); err == nil {
		t.Error("FromJSON accepted a document nested too deeply")
	}
}

func TestToJSONPutsIDFirst(t *testing.T) {
	out, err := ToJSON(map[string]any{"z": int64(1), "_id": "abc", "a": []any{2.5}})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"_id":"abc","a":[2.5],"z":1}` {
		t.Fatalf("ToJSON = %s", out)
	}
}
