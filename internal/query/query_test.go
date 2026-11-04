package query

import (
	"fmt"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Aliraza777/burrowdb/internal/doc"
	"github.com/Aliraza777/burrowdb/internal/value"
)

// The dataset covers the cases MongoDB's rules are about: missing vs null,
// ints vs floats, arrays of values and of objects, nested objects, and one
// field holding values of several types.
const dataset = `
{"_id":"a","name":"Ann","age":31,"tags":["admin","dev"],"address":{"city":"Lahore","zip":"54000"},"score":9.5,"active":true}
{"_id":"b","name":"Bob","age":27.5,"tags":["dev"],"address":{"city":"Karachi"},"score":7,"active":false}
{"_id":"c","name":"Cid","age":45,"tags":[],"address":{"city":"Lahore"},"active":true}
{"_id":"d","name":"Dee","age":"unknown","items":[{"sku":"x1","price":10},{"sku":"y2","price":25}],"active":true}
{"_id":"e","name":"Eve","age":null,"items":[{"sku":"x1","price":5}],"score":7.0}
{"_id":"f","name":"Fay","age":31.0,"tags":"solo","nested":{"a":{"b":{"c":1}}}}
{"_id":"g","name":"Gus","age":62,"tags":["admin"],"address":null,"score":-1}
{"_id":"h"}
{"_id":"i","name":"ann","age":18,"matrix":[[1,2],[3,4]],"active":false}
`

func loadDocs(t testing.TB) []map[string]any {
	t.Helper()
	var docs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(dataset), "\n") {
		d, err := doc.FromJSON([]byte(line))
		if err != nil {
			t.Fatal(err)
		}
		docs = append(docs, d)
	}
	return docs
}

func openWith(t testing.TB, docs []map[string]any) *doc.DB {
	t.Helper()
	db, err := doc.Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.InsertMany("c", docs); err != nil {
		t.Fatal(err)
	}
	return db
}

func ids(docs []map[string]any) string {
	var out []string
	for _, d := range docs {
		out = append(out, d["_id"].(string))
	}
	return strings.Join(out, ",")
}

// filterCases are checked against the dataset, with and without indexes.
var filterCases = []struct{ filter, want string }{
	{`{}`, "a,b,c,d,e,f,g,h,i"},
	{`{"name":"Ann"}`, "a"},
	{`{"name":"ann"}`, "i"},
	// 31 and 31.0 are equal, as in MongoDB.
	{`{"age":31}`, "a,f"},
	{`{"age":31.0}`, "a,f"},
	{`{"age":{"$eq":"unknown"}}`, "d"},
	// Range operators only compare within a type: the string and null ages never match.
	{`{"age":{"$gt":30}}`, "a,c,f,g"},
	{`{"age":{"$gte":31,"$lt":45}}`, "a,f"},
	{`{"age":{"$lt":30}}`, "b,i"},
	{`{"age":{"$gt":"a"}}`, "d"},
	{`{"age":{"$gt":null}}`, ""},
	// null matches both null and missing.
	{`{"age":null}`, "e,h"},
	{`{"age":{"$in":[null]}}`, "e,h"},
	{`{"age":{"$exists":false}}`, "h"},
	{`{"age":{"$exists":true}}`, "a,b,c,d,e,f,g,i"},
	{`{"age":{"$ne":31}}`, "b,c,d,e,g,h,i"},
	{`{"age":{"$in":[31,45]}}`, "a,c,f"},
	{`{"age":{"$nin":[31,45]}}`, "b,d,e,g,h,i"},
	// Arrays match on any element, or as a whole.
	{`{"tags":"admin"}`, "a,g"},
	{`{"tags":"solo"}`, "f"},
	{`{"tags":["admin","dev"]}`, "a"},
	{`{"tags":["dev","admin"]}`, ""},
	{`{"tags":[]}`, "c"},
	{`{"tags":{"$exists":true}}`, "a,b,c,f,g"},
	{`{"tags":{"$ne":"admin"}}`, "b,c,d,e,f,h,i"},
	{`{"tags":{"$in":["dev","solo"]}}`, "a,b,f"},
	{`{"tags.0":"admin"}`, "a,g"},
	{`{"tags.1":"dev"}`, "a"},
	// Nested fields and whole-object equality.
	{`{"address.city":"Lahore"}`, "a,c"},
	{`{"address.city":{"$ne":"Lahore"}}`, "b,d,e,f,g,h,i"},
	{`{"address.zip":{"$exists":true}}`, "a"},
	{`{"address":null}`, "d,e,f,g,h,i"},
	{`{"address":{"city":"Karachi"}}`, "b"},
	{`{"address":{"city":"Lahore"}}`, "c"},
	{`{"nested.a.b.c":1}`, "f"},
	{`{"nested.a.b":{"c":1}}`, "f"},
	{`{"missing.deep.path":null}`, "a,b,c,d,e,f,g,h,i"},
	// Paths through arrays of objects.
	{`{"items.sku":"x1"}`, "d,e"},
	{`{"items.price":{"$gt":20}}`, "d"},
	{`{"items.price":{"$lt":6}}`, "e"},
	{`{"items.0.price":10}`, "d"},
	{`{"items.1.sku":"y2"}`, "d"},
	{`{"items.price":{"$gte":5,"$lte":10}}`, "d,e"},
	// Without $elemMatch, each condition may be met by a different element:
	// d has 25 (> 10) and 10 (< 20), so it matches, as in MongoDB.
	{`{"items.price":{"$gt":10,"$lt":20}}`, "d"},
	// Arrays of arrays.
	{`{"matrix":[1,2]}`, "i"},
	{`{"matrix":[[1,2],[3,4]]}`, "i"},
	// Booleans and numbers.
	{`{"active":true}`, "a,c,d"},
	{`{"active":false}`, "b,i"},
	{`{"active":{"$ne":true}}`, "b,e,f,g,h,i"},
	{`{"score":7}`, "b,e"},
	{`{"score":{"$gt":0}}`, "a,b,e"},
	{`{"score":{"$lte":-1}}`, "g"},
	// Strings compare byte-wise, so lowercase sorts after uppercase.
	{`{"name":{"$gt":"D"}}`, "d,e,f,g,i"},
	{`{"name":{"$gte":"Bob","$lt":"Eve"}}`, "b,c,d"},
	// Logical operators.
	{`{"$or":[{"name":"Ann"},{"age":{"$gt":60}}]}`, "a,g"},
	{`{"$and":[{"active":true},{"address.city":"Lahore"}]}`, "a,c"},
	{`{"$nor":[{"active":true},{"active":false}]}`, "e,f,g,h"},
	{`{"active":true,"age":{"$gt":40}}`, "c"},
	{`{"$or":[{"tags":"admin"},{"tags":"solo"}],"score":{"$exists":true}}`, "a,g"},
	{`{"$or":[{"nobody":1}]}`, ""},
	{`{"$and":[{"$or":[{"age":18},{"age":62}]},{"$nor":[{"name":"Gus"}]}]}`, "i"},
}

func TestFilters(t *testing.T) {
	db := openWith(t, loadDocs(t))
	for _, c := range filterCases {
		q, err := ParseQueryJSON([]byte(`{"filter":` + c.filter + `}`))
		if err != nil {
			t.Errorf("%s: %v", c.filter, err)
			continue
		}
		got, err := Find(db, "c", q)
		if err != nil {
			t.Fatal(err)
		}
		if ids(got) != c.want {
			t.Errorf("%s\n   got  %q\n   want %q", c.filter, ids(got), c.want)
		}
		if n, _ := Count(db, "c", q.Filter); n != len(got) {
			t.Errorf("%s: Count %d, Find %d", c.filter, n, len(got))
		}
	}
}

func TestSort(t *testing.T) {
	db := openWith(t, loadDocs(t))
	cases := []struct{ sort, want string }{
		// null/missing < numbers < strings; ties keep _id order (a before f).
		{`{"age":1}`, "e,h,i,b,a,f,c,g,d"},
		{`{"age":-1}`, "d,g,c,a,f,b,i,e,h"},
		// Arrays sort by their smallest element ascending; an empty array counts as null.
		{`{"tags":1}`, "c,d,e,h,i,a,g,b,f"},
		// Booleans sort above everything; then name breaks ties.
		{`{"active":-1,"name":1}`, "a,c,d,b,i,h,e,f,g"},
		{`{"address.city":1,"_id":-1}`, "i,h,g,f,e,d,b,c,a"},
	}
	for _, c := range cases {
		q, err := ParseQueryJSON([]byte(`{"sort":` + c.sort + `}`))
		if err != nil {
			t.Fatal(err)
		}
		got, _ := Find(db, "c", q)
		if ids(got) != c.want {
			t.Errorf("sort %s\n   got  %s\n   want %s", c.sort, ids(got), c.want)
		}
	}
}

func TestSkipLimitProjection(t *testing.T) {
	db := openWith(t, loadDocs(t))
	q, err := ParseQueryJSON([]byte(`{"filter":{"age":{"$exists":true}},"sort":{"age":1},"skip":2,"limit":3,"projection":{"name":1,"address.city":1}}`))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := Find(db, "c", q)
	want := []map[string]any{
		{"_id": "b", "name": "Bob", "address": map[string]any{"city": "Karachi"}},
		{"_id": "a", "name": "Ann", "address": map[string]any{"city": "Lahore"}},
		{"_id": "f", "name": "Fay"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v", got)
	}

	q, _ = ParseQueryJSON([]byte(`{"filter":{"_id":"a"},"projection":{"_id":0,"tags":0,"address.zip":0}}`))
	got, _ = Find(db, "c", q)
	if !reflect.DeepEqual(got[0], map[string]any{
		"name": "Ann", "age": int64(31), "score": 9.5, "active": true, "address": map[string]any{"city": "Lahore"},
	}) {
		t.Fatalf("exclusion projection: %v", got[0])
	}

	q, _ = ParseQueryJSON([]byte(`{"filter":{"_id":"a"},"projection":{"_id":1}}`))
	got, _ = Find(db, "c", q)
	if !reflect.DeepEqual(got[0], map[string]any{"_id": "a"}) {
		t.Fatalf("_id-only projection: %v", got[0])
	}
	q, _ = ParseQueryJSON([]byte(`{"filter":{"_id":"h"},"projection":{}}`))
	got, _ = Find(db, "c", q)
	if !reflect.DeepEqual(got[0], map[string]any{"_id": "h"}) {
		t.Fatalf("empty projection should keep the whole document: %v", got[0])
	}
	// Skip past the end, and a limit with no sort.
	q, _ = ParseQueryJSON([]byte(`{"skip":100}`))
	if got, _ := Find(db, "c", q); len(got) != 0 {
		t.Fatal("skip past the end returned documents")
	}
	q, _ = ParseQueryJSON([]byte(`{"limit":2}`))
	if got, _ := Find(db, "c", q); ids(got) != "a,b" {
		t.Fatalf("limit 2 = %s", ids(got))
	}
}

func TestBadQueries(t *testing.T) {
	bad := []string{
		`{"filter":{"age":{"$foo":1}}}`,
		`{"filter":{"$where":"x"}}`,
		`{"filter":{"age":{"$gt":1,"plain":2}}}`,
		`{"filter":{"age":{"$in":5}}}`,
		`{"filter":{"age":{"$exists":"yes"}}}`,
		`{"filter":{"$or":[]}}`,
		`{"filter":{"$or":{"a":1}}}`,
		`{"filter":{"$and":[1]}}`,
		`{"filter":{"a..b":1}}`,
		`{"filter":[1]}`,
		`{"sort":{"age":2}}`,
		`{"sort":[["age",1]]}`,
		`{"projection":{"a":1,"b":0}}`,
		`{"projection":{"a":"yes"}}`,
		`{"unknown":1}`,
		`not json`,
	}
	for _, b := range bad {
		if _, err := ParseQueryJSON([]byte(b)); err == nil {
			t.Errorf("accepted %s", b)
		}
	}
	db := openWith(t, nil)
	if _, err := Find(db, "c", Query{Limit: -1}); err == nil {
		t.Error("negative limit accepted")
	}
}

func TestNaN(t *testing.T) {
	nan := math.NaN()
	if !value.Equal(nan, nan) || value.Equal(nan, 1.0) || value.Compare(nan, math.Inf(-1)) >= 0 {
		t.Fatal("NaN must equal only NaN and sort below every number")
	}
}

// --- properties over random documents and filters ---

var fields = []string{"a", "b", "c.d"}

func randValue(r *rand.Rand, depth int) any {
	switch r.IntN(9) {
	case 0:
		return nil
	case 1:
		return r.IntN(2) == 0
	case 2:
		return int64(r.IntN(5))
	case 3:
		return float64(r.IntN(5)) / 2
	case 4:
		return []string{"x", "y", "z"}[r.IntN(3)]
	case 5, 6:
		if depth > 1 {
			return int64(1)
		}
		arr := make([]any, r.IntN(3))
		for i := range arr {
			arr[i] = randValue(r, depth+1)
		}
		return arr
	default:
		if depth > 1 {
			return "x"
		}
		return map[string]any{"d": randValue(r, depth+1)}
	}
}

func randDoc(r *rand.Rand, i int) map[string]any {
	d := map[string]any{"_id": fmt.Sprintf("%04d", i)}
	for _, f := range []string{"a", "b", "c"} {
		if r.IntN(4) > 0 {
			d[f] = randValue(r, 0)
		}
	}
	return d
}

func randScalar(r *rand.Rand) any {
	for {
		v := randValue(r, 2)
		if _, isArr := v.([]any); !isArr {
			return v
		}
	}
}

func filter(t testing.TB, m map[string]any) *Filter {
	t.Helper()
	f, err := ParseFilter(m)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func randAtom(r *rand.Rand) map[string]any {
	field := fields[r.IntN(len(fields))]
	ops := []string{"$eq", "$ne", "$gt", "$gte", "$lt", "$lte", "$in", "$nin", "$exists"}
	op := ops[r.IntN(len(ops))]
	var arg any = randScalar(r)
	switch op {
	case "$in", "$nin":
		arg = []any{randScalar(r), randScalar(r)}
	case "$exists":
		arg = r.IntN(2) == 0
	}
	return map[string]any{field: map[string]any{op: arg}}
}

func TestFilterProperties(t *testing.T) {
	r := rand.New(rand.NewPCG(11, 12))
	docs := make([]map[string]any, 400)
	for i := range docs {
		docs[i] = randDoc(r, i)
	}
	for range 3000 {
		field := fields[r.IntN(len(fields))]
		v := randScalar(r)
		list := []any{randScalar(r), randScalar(r), randScalar(r)}
		ma, mb := randAtom(r), randAtom(r)
		or := filter(t, map[string]any{"$or": []any{ma, mb}})
		and := filter(t, map[string]any{"$and": []any{ma, mb}})
		nor := filter(t, map[string]any{"$nor": []any{ma, mb}})
		fa, fb := filter(t, ma), filter(t, mb)
		eq := filter(t, map[string]any{field: map[string]any{"$eq": v}})
		ne := filter(t, map[string]any{field: map[string]any{"$ne": v}})
		in := filter(t, map[string]any{field: map[string]any{"$in": list}})
		nin := filter(t, map[string]any{field: map[string]any{"$nin": list}})
		ex := filter(t, map[string]any{field: map[string]any{"$exists": true}})
		nex := filter(t, map[string]any{field: map[string]any{"$exists": false}})
		gt := filter(t, map[string]any{field: map[string]any{"$gt": v}})
		lte := filter(t, map[string]any{field: map[string]any{"$lte": v}})
		for _, d := range docs {
			check := func(name string, got, want bool) {
				if got != want {
					t.Fatalf("%s failed on %v (field %s, value %v, list %v, a %v, b %v)", name, d, field, v, list, ma, mb)
				}
			}
			check("$ne is the opposite of $eq", ne.Match(d), !eq.Match(d))
			check("$nin is the opposite of $in", nin.Match(d), !in.Match(d))
			inByEq := false
			for _, item := range list {
				inByEq = inByEq || filter(t, map[string]any{field: item}).Match(d)
			}
			check("$in is an OR of equalities", in.Match(d), inByEq)
			check("$or", or.Match(d), fa.Match(d) || fb.Match(d))
			check("$and", and.Match(d), fa.Match(d) && fb.Match(d))
			check("$nor", nor.Match(d), !(fa.Match(d) || fb.Match(d)))
			check("$exists false is the opposite of true", nex.Match(d), !ex.Match(d))
			// For a single non-array value of the same type, $gt and $lte split it exactly.
			vals := value.Lookup(d, value.SplitPath(field))
			if len(vals) == 1 && value.Comparable(vals[0], v) {
				check("$gt and $lte are complements on one scalar", gt.Match(d), !lte.Match(d))
			}
		}
	}
}

func TestFindProperties(t *testing.T) {
	r := rand.New(rand.NewPCG(13, 14))
	docs := make([]map[string]any, 300)
	for i := range docs {
		docs[i] = randDoc(r, i)
	}
	db := openWith(t, docs)
	for range 200 {
		f := filter(t, randAtom(r))
		var want []map[string]any
		for _, d := range docs {
			if f.Match(d) {
				want = append(want, d)
			}
		}
		all, _ := Find(db, "c", Query{Filter: f})
		if ids(all) != ids(want) {
			t.Fatalf("Find differs from matching each document")
		}

		sortBy := []SortField{{Path: fields[r.IntN(3)], Desc: r.IntN(2) == 0}, {Path: "_id"}}
		sorted, _ := Find(db, "c", Query{Filter: f, Sort: sortBy})
		if len(sorted) != len(want) {
			t.Fatal("sorting changed the result size")
		}
		for i := 1; i < len(sorted); i++ {
			c := value.Compare(sortValue(sorted[i-1], sortBy[0]), sortValue(sorted[i], sortBy[0]))
			if sortBy[0].Desc {
				c = -c
			}
			if c > 0 || (c == 0 && sorted[i-1]["_id"].(string) > sorted[i]["_id"].(string)) {
				t.Fatalf("not sorted at %d by %v", i, sortBy)
			}
		}
		skip, limit := r.IntN(10), r.IntN(10)
		page, _ := Find(db, "c", Query{Filter: f, Sort: sortBy, Skip: skip, Limit: limit})
		end := len(sorted)
		if limit > 0 {
			end = min(end, skip+limit)
		}
		if ids(page) != ids(sorted[min(skip, len(sorted)):max(end, min(skip, len(sorted)))]) {
			t.Fatalf("skip %d limit %d is not a slice of the full result", skip, limit)
		}
		unsortedPage, _ := Find(db, "c", Query{Filter: f, Skip: skip, Limit: limit})
		uend := len(all)
		if limit > 0 {
			uend = min(uend, skip+limit)
		}
		if ids(unsortedPage) != ids(all[min(skip, len(all)):max(uend, min(skip, len(all)))]) {
			t.Fatalf("early-stopping scan with skip %d limit %d differs", skip, limit)
		}
	}
}

// Documents that tie on every sort key keep _id order. Uses enough documents
// that the sort can't fall back to insertion sort, which is stable by accident.
func TestSortIsStable(t *testing.T) {
	var docs []map[string]any
	for i := range 500 {
		docs = append(docs, map[string]any{"_id": fmt.Sprintf("%04d", i), "group": int64(i % 3)})
	}
	db := openWith(t, docs)
	got, err := Find(db, "c", Query{Sort: []SortField{{Path: "group"}}})
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1]["group"] == got[i]["group"] && got[i-1]["_id"].(string) > got[i]["_id"].(string) {
			t.Fatalf("ties out of _id order at %d: %v then %v", i, got[i-1]["_id"], got[i]["_id"])
		}
	}
}

// TestMillionScan times queries over 1,000,000 documents with a full scan,
// the baseline phase 5's indexes should beat. Opt-in:
// BURROWDB_MILLION=1 go test -run TestMillionScan -v ./internal/query
func TestMillionScan(t *testing.T) {
	if os.Getenv("BURROWDB_MILLION") == "" {
		t.Skip("set BURROWDB_MILLION=1 to run")
	}
	db, err := doc.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	batch := make([]map[string]any, 5000)
	for done := 0; done < 1_000_000; done += len(batch) {
		for i := range batch {
			n := done + i
			batch[i] = map[string]any{"n": int64(n), "age": int64(n % 90), "city": []string{"Lahore", "Karachi", "Multan"}[n%3]}
		}
		if _, err := db.InsertMany("users", batch); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	for _, f := range []string{"n", "age"} {
		if err := db.CreateIndex("users", f); err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("built indexes on n and age in %v", time.Since(start).Round(time.Millisecond))
	for _, qs := range []string{
		`{"filter":{"n":123456}}`,
		`{"filter":{"n":{"$gte":500000,"$lt":500100}}}`,
		`{"filter":{"age":{"$gte":30,"$lt":32},"city":"Lahore"}}`,
		`{"filter":{"age":42},"sort":{"n":-1},"limit":10}`,
	} {
		q, err := ParseQueryJSON([]byte(qs))
		if err != nil {
			t.Fatal(err)
		}
		var times [2]time.Duration
		var results [2]string
		for i, hint := range []string{"$natural", ""} {
			q.Hint = hint
			start := time.Now()
			got, err := Find(db, "users", q)
			if err != nil {
				t.Fatal(err)
			}
			times[i], results[i] = time.Since(start), ids(got)
		}
		if results[0] != results[1] {
			t.Fatalf("%s: index and scan disagree", qs)
		}
		ex, _ := ExplainQuery(db, "users", q)
		t.Logf("%-58s %5d results  scan %7v  %s %7v  (%.0fx)", qs, ex.Returned,
			times[0].Round(time.Millisecond), ex.Plan, times[1].Round(10*time.Microsecond),
			float64(times[0])/float64(times[1]))
	}
}
