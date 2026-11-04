package btree

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
)

// memPages is an in-memory Pages that also catches misuse: reading a freed
// page, freeing twice, or storing a page of the wrong size.
type memPages struct {
	t     testing.TB
	pages map[uint64][]byte
	next  uint64
}

func newMem(t testing.TB) *memPages { return &memPages{t: t, pages: map[uint64][]byte{}, next: 1} }

func (m *memPages) Get(ptr uint64) []byte {
	p, ok := m.pages[ptr]
	if !ok {
		m.t.Fatalf("read of missing page %d", ptr)
	}
	return p
}

func (m *memPages) New(p []byte) uint64 {
	if len(p) != PageSize {
		m.t.Fatalf("New: page of %d bytes", len(p))
	}
	ptr := m.next
	m.next++
	m.pages[ptr] = p
	return ptr
}

func (m *memPages) Del(ptr uint64) {
	if _, ok := m.pages[ptr]; !ok {
		m.t.Fatalf("double free of page %d", ptr)
	}
	delete(m.pages, ptr)
}

// model is the reference the tree is compared against.
type model map[string]string

// check compares the tree with the model: same keys in the same order, a
// valid structure, and exactly the live pages stored (no leaks).
func check(t testing.TB, tr *Tree, m *memPages, want model) {
	t.Helper()
	reachable := map[uint64]bool{}
	if err := tr.Verify(func(p uint64) { reachable[p] = true }); err != nil {
		t.Fatal(err)
	}
	if len(reachable) != len(m.pages) {
		t.Fatalf("%d pages stored but %d reachable: pages leaked", len(m.pages), len(reachable))
	}
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	i := 0
	for it := tr.Seek(nil); it.Valid(); it.Next() {
		if i >= len(keys) || string(it.Key()) != keys[i] || string(it.Value()) != want[keys[i]] {
			t.Fatalf("scan position %d: got %q=%q", i, it.Key(), it.Value())
		}
		i++
	}
	if i != len(keys) {
		t.Fatalf("scan found %d keys, want %d", i, len(keys))
	}
	for _, k := range keys {
		v, ok := tr.Get([]byte(k))
		if !ok || string(v) != want[k] {
			t.Fatalf("Get(%q) = %q, %v; want %q", k, v, ok, want[k])
		}
	}
}

func key(i int) []byte { return []byte(fmt.Sprintf("key-%08d", i)) }

func TestSetGetDeleteSmall(t *testing.T) {
	m := newMem(t)
	tr := &Tree{Pages: m}
	want := model{}
	if _, ok := tr.Get([]byte("x")); ok {
		t.Fatal("empty tree returned a value")
	}
	for _, kv := range [][2]string{{"b", "2"}, {"a", "1"}, {"c", "3"}, {"b", "22"}} {
		if err := tr.Set([]byte(kv[0]), []byte(kv[1])); err != nil {
			t.Fatal(err)
		}
		want[kv[0]] = kv[1]
	}
	check(t, tr, m, want)
	if ok, _ := tr.Delete([]byte("a")); !ok {
		t.Fatal("Delete(a) = false")
	}
	if ok, _ := tr.Delete([]byte("zzz")); ok {
		t.Fatal("Delete of a missing key returned true")
	}
	delete(want, "a")
	check(t, tr, m, want)
}

func TestRejectsBadKeys(t *testing.T) {
	tr := &Tree{Pages: newMem(t)}
	for _, c := range []struct {
		k, v []byte
		err  error
	}{
		{nil, nil, ErrEmptyKey},
		{make([]byte, MaxKeySize+1), nil, ErrKeyTooLarge},
		{[]byte("k"), make([]byte, MaxValueSize+1), ErrValueTooLarge},
	} {
		if err := tr.Set(c.k, c.v); !errors.Is(err, c.err) {
			t.Errorf("Set: %v, want %v", err, c.err)
		}
	}
	// The largest allowed pair must fit.
	if err := tr.Set(bytes.Repeat([]byte("k"), MaxKeySize), make([]byte, MaxValueSize)); err != nil {
		t.Fatal(err)
	}
}

// Enough keys for several levels, then delete everything again, checking the
// structure and page accounting along the way.
func TestGrowAndShrink(t *testing.T) {
	m := newMem(t)
	tr := &Tree{Pages: m}
	want := model{}
	const n = 20000
	order := rand.New(rand.NewPCG(1, 2)).Perm(n)
	for i, k := range order {
		v := bytes.Repeat([]byte{byte(k)}, k%200)
		if err := tr.Set(key(k), v); err != nil {
			t.Fatal(err)
		}
		want[string(key(k))] = string(v)
		if i%2500 == 0 {
			check(t, tr, m, want)
		}
	}
	check(t, tr, m, want)
	for i, k := range order {
		if ok, err := tr.Delete(key(k)); !ok || err != nil {
			t.Fatalf("Delete(%s) = %v, %v", key(k), ok, err)
		}
		delete(want, string(key(k)))
		if i%2500 == 0 {
			check(t, tr, m, want)
		}
	}
	check(t, tr, m, want)
	if len(m.pages) != 1 {
		t.Fatalf("empty tree holds %d pages, want 1 (the root leaf)", len(m.pages))
	}
}

// Large values force 3-way splits and merges with few keys per node.
func TestLargeValues(t *testing.T) {
	m := newMem(t)
	tr := &Tree{Pages: m}
	want := model{}
	r := rand.New(rand.NewPCG(3, 4))
	for i := range 3000 {
		k := key(r.IntN(800))
		if i%3 == 2 {
			tr.Delete(k)
			delete(want, string(k))
			continue
		}
		v := bytes.Repeat([]byte{'v'}, r.IntN(MaxValueSize+1))
		tr.Set(k, v)
		want[string(k)] = string(v)
	}
	check(t, tr, m, want)
}

func TestSeek(t *testing.T) {
	tr := &Tree{Pages: newMem(t)}
	for i := 0; i < 1000; i += 2 {
		tr.Set(key(i), nil)
	}
	cases := []struct {
		seek string
		want string // "" means past the end
	}{
		{"", "key-00000000"},
		{"key-00000000", "key-00000000"},
		{"key-00000001", "key-00000002"},
		{"key-00000500", "key-00000500"},
		{"key-00000997", "key-00000998"},
		{"key-00000999", ""},
		{"zzz", ""},
	}
	for _, c := range cases {
		it := tr.Seek([]byte(c.seek))
		got := ""
		if it.Valid() {
			got = string(it.Key())
		}
		if got != c.want {
			t.Errorf("Seek(%q) at %q, want %q", c.seek, got, c.want)
		}
	}
	// A range scan: keys in [100, 120).
	var got []string
	for it := tr.Seek(key(100)); it.Valid() && bytes.Compare(it.Key(), key(120)) < 0; it.Next() {
		got = append(got, string(it.Key()))
	}
	if len(got) != 10 || got[0] != "key-00000100" || got[9] != "key-00000118" {
		t.Fatalf("range scan = %v", got)
	}
}

// FuzzTree runs random operations against the tree and the model. The input
// bytes are decoded as a sequence of (op, key, value length) triples.
func FuzzTree(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9})
	f.Add(bytes.Repeat([]byte{0, 200, 255, 1, 100, 250}, 200))
	f.Fuzz(func(t *testing.T, data []byte) {
		m := newMem(t)
		tr := &Tree{Pages: m}
		want := model{}
		for len(data) >= 4 {
			// Value sizes span 0..MaxValueSize so large values force multi-way splits.
			op, k, vlen := data[0], binary.LittleEndian.Uint16(data[1:3])%512, int(data[3])*MaxValueSize/255
			data = data[4:]
			kb := key(int(k))
			if op%4 == 0 {
				tr.Delete(kb)
				delete(want, string(kb))
				continue
			}
			v := bytes.Repeat([]byte{op}, vlen)
			if err := tr.Set(kb, v); err != nil {
				t.Fatal(err)
			}
			want[string(kb)] = string(v)
		}
		check(t, tr, m, want)
	})
}

func BenchmarkSet(b *testing.B) {
	tr := &Tree{Pages: newMem(b)}
	v := make([]byte, 100)
	r := rand.New(rand.NewPCG(5, 6))
	for b.Loop() {
		tr.Set(key(r.IntN(1_000_000)), v)
	}
}

func BenchmarkGet(b *testing.B) {
	tr := &Tree{Pages: newMem(b)}
	for i := range 100_000 {
		tr.Set(key(i), make([]byte, 100))
	}
	r := rand.New(rand.NewPCG(7, 8))
	for b.Loop() {
		tr.Get(key(r.IntN(100_000)))
	}
}
