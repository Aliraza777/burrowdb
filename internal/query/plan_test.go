package query

import (
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aliraza777/burrowdb/internal/doc"
)

// Every field the filter cases touch.
var indexedFields = []string{
	"name", "age", "tags", "tags.0", "tags.1", "address", "address.city", "address.zip",
	"nested.a.b", "nested.a.b.c", "missing.deep.path", "items.sku", "items.price",
	"items.0.price", "items.1.sku", "matrix", "active", "score",
}

// usableHints returns the indexed fields a filter has a usable condition on.
func usableHints(f *Filter, indexed []string) []string {
	var out []string
	for _, fn := range conjuncts(f.root) {
		field := strings.Join(fn.path, ".")
		for _, ix := range indexed {
			if ix == field {
				if _, _, ok := rangesFor(fn.ops, true); ok {
					out = append(out, field)
				}
				break
			}
		}
	}
	return out
}

// The hand-checked filter cases must give the same answers through every
// plan: the planner's choice, a forced scan, and each usable index forced.
func TestFiltersWithIndexes(t *testing.T) {
	db := openWith(t, loadDocs(t))
	for _, f := range indexedFields {
		if err := db.CreateIndex("c", f); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.CheckIndexes(); err != nil {
		t.Fatal(err)
	}
	plansUsed := 0
	for _, c := range filterCases {
		q, err := ParseQueryJSON([]byte(`{"filter":` + c.filter + `}`))
		if err != nil {
			t.Fatal(err)
		}
		hints := append([]string{"", "$natural"}, usableHints(q.Filter, indexedFields)...)
		for _, h := range hints {
			q.Hint = h
			got, err := Find(db, "c", q)
			if err != nil {
				t.Fatalf("%s hint %q: %v", c.filter, h, err)
			}
			if ids(got) != c.want {
				t.Errorf("%s with hint %q\n   got  %q\n   want %q", c.filter, h, ids(got), c.want)
			}
			if h != "" && h != "$natural" {
				plansUsed++
			}
		}
	}
	if plansUsed < 40 {
		t.Fatalf("only %d forced index plans ran; the test isn't exercising indexes", plansUsed)
	}
}

// Random documents and filters: every usable index must agree with a scan.
func TestIndexPlansAgreeWithScan(t *testing.T) {
	r := rand.New(rand.NewPCG(21, 22))
	docs := make([]map[string]any, 400)
	for i := range docs {
		docs[i] = randDoc(r, i)
	}
	db := openWith(t, docs)
	indexed := []string{"a", "b", "c.d"}
	for _, f := range indexed {
		db.CreateIndex("c", f)
	}
	compared := 0
	for i := range 1500 {
		m := randAtom(r)
		if i%2 == 0 {
			m = map[string]any{"$and": []any{randAtom(r), randAtom(r)}}
		}
		f := filter(t, m)
		want, _ := Find(db, "c", Query{Filter: f, Hint: "$natural"})
		auto, _ := Find(db, "c", Query{Filter: f})
		if ids(auto) != ids(want) {
			t.Fatalf("planner's choice differs from a scan for %v", m)
		}
		for _, h := range usableHints(f, indexed) {
			got, err := Find(db, "c", Query{Filter: f, Hint: h})
			if err != nil {
				t.Fatal(err)
			}
			if ids(got) != ids(want) {
				t.Fatalf("index %s differs from a scan for %v\n got  %s\n want %s", h, m, ids(got), ids(want))
			}
			compared++
		}
	}
	if compared < 500 {
		t.Fatalf("only %d index plans compared", compared)
	}
}

func TestExplain(t *testing.T) {
	db, err := doc.Open(filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var batch []map[string]any
	for i := range 1000 {
		batch = append(batch, map[string]any{"n": int64(i), "even": i%2 == 0, "city": []string{"a", "b"}[i%2]})
	}
	db.InsertMany("c", batch)
	db.CreateIndex("c", "n")
	db.CreateIndex("c", "even")

	explain := func(qs string) Explain {
		t.Helper()
		q, err := ParseQueryJSON([]byte(qs))
		if err != nil {
			t.Fatal(err)
		}
		ex, err := ExplainQuery(db, "c", q)
		if err != nil {
			t.Fatal(err)
		}
		return ex
	}
	ex := explain(`{"filter":{"n":500}}`)
	if ex.Plan != "IXSCAN" || ex.Index != "n" || ex.DocsExamined != 1 || ex.Returned != 1 || ex.KeysExamined != 1 {
		t.Fatalf("point lookup: %+v", ex)
	}
	ex = explain(`{"filter":{"n":{"$gte":100,"$lt":110}}}`)
	if ex.Plan != "IXSCAN" || ex.DocsExamined != 10 || ex.Returned != 10 {
		t.Fatalf("range: %+v", ex)
	}
	// "even" matches half the collection, so the selective "n" index wins.
	ex = explain(`{"filter":{"even":true,"n":{"$lt":20}}}`)
	if ex.Index != "n" || ex.Returned != 10 || len(ex.Considered) != 2 {
		t.Fatalf("should pick the selective index: %+v", ex)
	}
	// An index that reads half the collection loses to a scan.
	if ex = explain(`{"filter":{"even":true}}`); ex.Plan != "COLLSCAN" || ex.Returned != 500 {
		t.Fatalf("unselective index should lose to a scan: %+v", ex)
	}
	// $ne can't use an index; neither can an unindexed field.
	if ex = explain(`{"filter":{"n":{"$ne":5}}}`); ex.Plan != "COLLSCAN" || ex.DocsExamined != 1000 {
		t.Fatalf("$ne: %+v", ex)
	}
	if ex = explain(`{"filter":{"city":"a"}}`); ex.Plan != "COLLSCAN" {
		t.Fatalf("unindexed field: %+v", ex)
	}
	// With a limit and no sort, an index plan stops early too.
	if ex = explain(`{"filter":{"n":{"$gte":0}},"limit":3,"hint":"n"}`); ex.DocsExamined != 3 || ex.Returned != 3 {
		t.Fatalf("limit should stop fetching early: %+v", ex)
	}
	// Hints.
	if ex = explain(`{"filter":{"n":500},"hint":"$natural"}`); ex.Plan != "COLLSCAN" {
		t.Fatalf("$natural hint: %+v", ex)
	}
	if ex = explain(`{"filter":{"even":true},"hint":"even"}`); ex.Index != "even" || ex.Returned != 500 {
		t.Fatalf("forced index: %+v", ex)
	}
	q, _ := ParseQueryJSON([]byte(`{"filter":{"n":500},"hint":"city"}`))
	if _, err := Find(db, "c", q); err == nil {
		t.Fatal("hint on a field without an index was accepted")
	}
	q, _ = ParseQueryJSON([]byte(`{"filter":{"n":{"$ne":1}},"hint":"n"}`))
	if _, err := Find(db, "c", q); err == nil {
		t.Fatal("hint the filter can't use was accepted")
	}
	// An empty range ($gt 10 and $lt 5) reads nothing.
	if ex = explain(`{"filter":{"n":{"$gt":10,"$lt":5}}}`); ex.Plan != "IXSCAN" || ex.KeysExamined != 0 || ex.Returned != 0 {
		t.Fatalf("empty range: %+v", ex)
	}
	_ = fmt.Sprint
}
