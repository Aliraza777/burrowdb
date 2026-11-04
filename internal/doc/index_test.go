package doc

import (
	"errors"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/Aliraza777/burrowdb/internal/value"
)

func checkIndexes(t testing.TB, db *DB) {
	t.Helper()
	if err := db.CheckIndexes(); err != nil {
		t.Fatal(err)
	}
	checkPages(t, db)
}

func scanIDs(t testing.TB, db *DB, coll, field string, lo, hi any) []string {
	t.Helper()
	var out []string
	err := db.Read(func(r *Reader) error {
		klo, _ := value.Key(lo)
		khi, _ := value.Key(hi)
		return r.ScanIndex(coll, field, klo, value.After(khi), func(id string) bool {
			out = append(out, id)
			return true
		})
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestCreateIndexAndRangeScan(t *testing.T) {
	db, _ := openDB(t)
	db.InsertMany("u", []map[string]any{
		{"_id": "a", "age": int64(31)},
		{"_id": "b", "age": 27.5},
		{"_id": "c", "age": int64(45)},
		{"_id": "d", "age": "unknown"},
		{"_id": "e"},
		{"_id": "f", "age": 31.0},
	})
	if err := db.CreateIndex("u", "age"); err != nil {
		t.Fatal(err)
	}
	checkIndexes(t, db)
	// Ordered by value; 31 and 31.0 share a key, so _id breaks the tie.
	if got := scanIDs(t, db, "u", "age", int64(0), int64(100)); !slices.Equal(got, []string{"b", "a", "f", "c"}) {
		t.Fatalf("numbers 0..100 = %v", got)
	}
	if got := scanIDs(t, db, "u", "age", nil, nil); !slices.Equal(got, []string{"e"}) {
		t.Fatalf("missing field should be indexed as null, got %v", got)
	}
	// Later writes keep the index in sync.
	db.Insert("u", map[string]any{"_id": "g", "age": int64(40)})
	db.Replace("u", "a", map[string]any{"age": int64(99)})
	db.Delete("u", "c")
	checkIndexes(t, db)
	if got := scanIDs(t, db, "u", "age", int64(0), int64(100)); !slices.Equal(got, []string{"b", "f", "g", "a"}) {
		t.Fatalf("after writes = %v", got)
	}

	if err := db.CreateIndex("u", "age"); !errors.Is(err, ErrIndexExists) {
		t.Fatalf("second CreateIndex: %v", err)
	}
	for _, bad := range []string{"", "_id", "$x", "a..b", ".a", "a."} {
		if err := db.CreateIndex("u", bad); !errors.Is(err, ErrBadField) {
			t.Errorf("CreateIndex(%q): %v", bad, err)
		}
	}
	idx, _ := db.Indexes("u")
	if len(idx) != 1 || idx[0].Field != "age" || idx[0].Multikey {
		t.Fatalf("Indexes = %+v", idx)
	}
	if err := db.DropIndex("u", "age"); err != nil {
		t.Fatal(err)
	}
	if err := db.DropIndex("u", "age"); !errors.Is(err, ErrIndexNotFound) {
		t.Fatalf("second DropIndex: %v", err)
	}
	checkIndexes(t, db)
}

func TestMultikeyIndex(t *testing.T) {
	db, _ := openDB(t)
	db.CreateIndex("p", "tags")
	db.CreateIndex("p", "items.price")
	idx, _ := db.Indexes("p")
	if idx[0].Multikey || idx[1].Multikey {
		t.Fatal("an empty index shouldn't start out multikey")
	}
	db.Insert("p", map[string]any{"_id": "x", "tags": "solo"})
	db.Insert("p", map[string]any{"_id": "y", "tags": []any{"a", "b", "a"}, "items": []any{
		map[string]any{"price": int64(10)}, map[string]any{"price": int64(25)},
	}})
	checkIndexes(t, db)
	idx, _ = db.Indexes("p")
	if !idx[0].Multikey || !idx[1].Multikey {
		t.Fatalf("arrays should make both indexes multikey: %+v", idx)
	}
	// One entry per distinct element.
	if got := scanIDs(t, db, "p", "tags", "a", "z"); !slices.Equal(got, []string{"y", "y", "x"}) {
		t.Fatalf("tags entries = %v", got)
	}
	if got := scanIDs(t, db, "p", "items.price", int64(20), int64(30)); !slices.Equal(got, []string{"y"}) {
		t.Fatalf("items.price 20..30 = %v", got)
	}
	db.Replace("p", "y", map[string]any{"tags": "plain"})
	checkIndexes(t, db)
}

func TestDropCollectionRemovesIndexes(t *testing.T) {
	db, _ := openDB(t)
	db.CreateIndex("gone", "n")
	for i := range 300 {
		db.Insert("gone", map[string]any{"n": int64(i), "pad": strings.Repeat("x", 200)})
	}
	db.Insert("kept", map[string]any{"n": int64(1)})
	db.CreateIndex("kept", "n")
	db.DropCollection("gone")
	checkIndexes(t, db)
	if idx, _ := db.Indexes("gone"); len(idx) != 0 {
		t.Fatalf("indexes survived the drop: %v", idx)
	}
	err := db.Read(func(r *Reader) error {
		for it := r.t.Seek(nil); it.Valid(); it.Next() {
			if strings.Contains(string(it.Key()), "gone") {
				t.Errorf("key %q survived the drop", it.Key())
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Random writes with several indexes; after each batch and each reopen the
// indexes must match the documents exactly.
func TestIndexesStayInSync(t *testing.T) {
	db, path := openDB(t)
	for _, f := range []string{"a", "b.c", "tags"} {
		if err := db.CreateIndex("r", f); err != nil {
			t.Fatal(err)
		}
	}
	r := rand.New(rand.NewPCG(5, 6))
	randVal := func() any {
		switch r.IntN(6) {
		case 0:
			return nil
		case 1:
			return int64(r.IntN(10))
		case 2:
			return float64(r.IntN(10)) / 2
		case 3:
			return []string{"x", "y", "z"}[r.IntN(3)]
		case 4:
			return []any{int64(r.IntN(3)), "x"}
		}
		return map[string]any{"c": int64(r.IntN(3))}
	}
	randDoc := func() map[string]any {
		d := map[string]any{}
		if r.IntN(3) > 0 {
			d["a"] = randVal()
		}
		if r.IntN(3) > 0 {
			d["b"] = randVal()
		}
		if r.IntN(2) > 0 {
			d["tags"] = randVal()
		}
		return d
	}
	var ids []string
	for step := range 1500 {
		switch op := r.IntN(10); {
		case op < 5 || len(ids) == 0:
			id, err := db.Insert("r", randDoc())
			if err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		case op < 8:
			if err := db.Replace("r", ids[r.IntN(len(ids))], randDoc()); err != nil {
				t.Fatal(err)
			}
		default:
			i := r.IntN(len(ids))
			db.Delete("r", ids[i])
			ids = slices.Delete(ids, i, i+1)
		}
		if step%300 == 299 {
			checkIndexes(t, db)
			db = reopen(t, db, path)
		}
	}
	// An index created late must equal one maintained all along.
	if err := db.CreateIndex("r", "b"); err != nil {
		t.Fatal(err)
	}
	checkIndexes(t, db)
}
