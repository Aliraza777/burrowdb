package doc

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func openDB(t testing.TB) (*DB, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "docs.db")
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db, path
}

func reopen(t testing.TB, db *DB, path string) *DB {
	t.Helper()
	db.Close()
	db2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db2.Close() })
	return db2
}

func checkPages(t testing.TB, db *DB) {
	t.Helper()
	if err := db.Pager().Check(); err != nil {
		t.Fatal(err)
	}
}

func scanAll(t testing.TB, db *DB, coll string) []map[string]any {
	t.Helper()
	var out []map[string]any
	if err := db.Scan(coll, func(d map[string]any) bool { out = append(out, d); return true }); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestInsertGetReplaceDelete(t *testing.T) {
	db, path := openDB(t)
	id, err := db.Insert("users", map[string]any{"name": "Ann", "age": int64(31)})
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 24 {
		t.Fatalf("generated _id %q", id)
	}
	got, err := db.Get("users", id)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"_id": id, "name": "Ann", "age": int64(31)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Get = %v", got)
	}

	if err := db.Replace("users", id, map[string]any{"name": "Ann", "age": int64(32)}); err != nil {
		t.Fatal(err)
	}
	if err := db.Replace("users", "missing-id", map[string]any{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Replace of a missing document: %v", err)
	}
	if err := db.Replace("users", id, map[string]any{"_id": "other"}); !errors.Is(err, ErrIDMismatch) {
		t.Fatalf("Replace with a different _id: %v", err)
	}

	db = reopen(t, db, path)
	got, _ = db.Get("users", id)
	if got["age"] != int64(32) {
		t.Fatalf("after replace and reopen: %v", got)
	}
	if n, _ := db.Count("users"); n != 1 {
		t.Fatalf("Count = %d", n)
	}
	if ok, _ := db.Delete("users", id); !ok {
		t.Fatal("Delete returned false")
	}
	if ok, _ := db.Delete("users", id); ok {
		t.Fatal("second Delete returned true")
	}
	if _, err := db.Get("users", id); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after Delete: %v", err)
	}
	if n, _ := db.Count("users"); n != 0 {
		t.Fatalf("Count after Delete = %d", n)
	}
	checkPages(t, db)
}

func TestCustomIDsAndDuplicates(t *testing.T) {
	db, _ := openDB(t)
	if _, err := db.Insert("c", map[string]any{"_id": "user-1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Insert("c", map[string]any{"_id": "user-1"}); !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("duplicate _id: %v", err)
	}
	for _, bad := range []any{"", "has space", "tab\t", int64(5), strings.Repeat("x", 129)} {
		if _, err := db.Insert("c", map[string]any{"_id": bad}); !errors.Is(err, ErrBadID) {
			t.Errorf("_id %q accepted: %v", bad, err)
		}
	}
	for _, bad := range []string{"", "a/b", "a b", "naïve", strings.Repeat("c", 65)} {
		if _, err := db.Insert(bad, map[string]any{}); !errors.Is(err, ErrBadCollection) {
			t.Errorf("collection %q accepted: %v", bad, err)
		}
	}
}

// One bad document in a batch means nothing from the batch is stored.
func TestInsertManyIsAtomic(t *testing.T) {
	db, _ := openDB(t)
	db.Insert("c", map[string]any{"_id": "taken"})
	_, err := db.InsertMany("c", []map[string]any{{"_id": "new-1"}, {"_id": "new-2"}, {"_id": "taken"}})
	if !errors.Is(err, ErrDuplicateID) {
		t.Fatalf("InsertMany: %v", err)
	}
	if _, err := db.Get("c", "new-1"); !errors.Is(err, ErrNotFound) {
		t.Fatal("part of a failed batch was stored")
	}
	if n, _ := db.Count("c"); n != 1 {
		t.Fatalf("Count = %d after failed batch", n)
	}
	checkPages(t, db)
}

// Documents larger than one B+tree value are split into chunks; growing and
// shrinking them must leave no stray chunks or pages.
func TestLargeDocuments(t *testing.T) {
	db, path := openDB(t)
	big := strings.Repeat("burrow ", 30_000) // ~210 KB, ~70 chunks
	id, err := db.Insert("files", map[string]any{"body": big, "n": int64(1)})
	if err != nil {
		t.Fatal(err)
	}
	db.Insert("files", map[string]any{"body": "neighbour"})
	got, _ := db.Get("files", id)
	if got["body"] != big {
		t.Fatal("large document came back different")
	}
	if err := db.Replace("files", id, map[string]any{"body": "small now"}); err != nil {
		t.Fatal(err)
	}
	db = reopen(t, db, path)
	got, _ = db.Get("files", id)
	if got["body"] != "small now" {
		t.Fatalf("after shrinking: %v", got["body"])
	}
	if docs := scanAll(t, db, "files"); len(docs) != 2 {
		t.Fatalf("scan found %d documents, want 2 (stray chunks?)", len(docs))
	}
	checkPages(t, db)

	huge := map[string]any{"x": strings.Repeat("y", MaxDocSize)}
	if _, err := db.Insert("files", huge); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized document: %v", err)
	}
}

func TestScanOrderAndCollections(t *testing.T) {
	db, _ := openDB(t)
	var ids []string
	for i := range 50 {
		id, err := db.Insert("events", map[string]any{"i": int64(i)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	db.Insert("events2", map[string]any{"other": true}) // name shares a prefix
	db.Insert("a", map[string]any{})

	docs := scanAll(t, db, "events")
	if len(docs) != 50 {
		t.Fatalf("scan found %d", len(docs))
	}
	for i, d := range docs {
		if d["i"] != int64(i) || d["_id"] != ids[i] {
			t.Fatalf("scan position %d: %v (generated ids should sort in insert order)", i, d)
		}
	}
	seen := 0
	db.Scan("events", func(map[string]any) bool { seen++; return seen < 5 })
	if seen != 5 {
		t.Fatalf("scan didn't stop early: %d", seen)
	}
	names, _ := db.Collections()
	if !slices.Equal(names, []string{"a", "events", "events2"}) {
		t.Fatalf("Collections() = %v", names)
	}

	existed, err := db.DropCollection("events")
	if err != nil || !existed {
		t.Fatalf("DropCollection = %v, %v", existed, err)
	}
	if docs := scanAll(t, db, "events"); len(docs) != 0 {
		t.Fatal("documents survived DropCollection")
	}
	if docs := scanAll(t, db, "events2"); len(docs) != 1 {
		t.Fatal("DropCollection touched another collection")
	}
	names, _ = db.Collections()
	if !slices.Equal(names, []string{"a", "events2"}) {
		t.Fatalf("Collections() after drop = %v", names)
	}
	checkPages(t, db)
}

func TestIDsAreUniqueAndOrdered(t *testing.T) {
	g := newIDGen()
	g.counter = 0 // avoid the rare wrap-around within this test
	clock := time.Unix(1_800_000_000, 0)
	g.now = func() time.Time { return clock }
	seen := map[string]bool{}
	prev := ""
	for i := range 20_000 {
		if i == 10_000 {
			clock = clock.Add(time.Second)
		}
		id := g.next()
		if seen[id] || id <= prev {
			t.Fatalf("id %d: %s duplicated or out of order after %s", i, id, prev)
		}
		seen[id], prev = true, id
	}
}

// Random operations against the database and an in-memory model, with
// reopening, then a full page check.
func TestAgainstModel(t *testing.T) {
	db, path := openDB(t)
	r := rand.New(rand.NewPCG(9, 9))
	want := map[string]map[string]any{} // _id -> document
	ids := []string{}
	for step := range 3000 {
		switch op := r.IntN(10); {
		case op < 5 || len(ids) == 0:
			d := map[string]any{"n": int64(step), "pad": strings.Repeat("p", r.IntN(9000))}
			id, err := db.Insert("m", d)
			if err != nil {
				t.Fatal(err)
			}
			d["_id"] = id
			want[id], ids = d, append(ids, id)
		case op < 7:
			id := ids[r.IntN(len(ids))]
			d := map[string]any{"_id": id, "replaced": int64(step), "pad": strings.Repeat("q", r.IntN(9000))}
			if err := db.Replace("m", id, d); err != nil {
				t.Fatal(err)
			}
			want[id] = d
		default:
			i := r.IntN(len(ids))
			if ok, err := db.Delete("m", ids[i]); !ok || err != nil {
				t.Fatalf("Delete(%s) = %v, %v", ids[i], ok, err)
			}
			delete(want, ids[i])
			ids = slices.Delete(ids, i, i+1)
		}
		if step%1000 == 999 {
			db = reopen(t, db, path)
		}
	}
	got := scanAll(t, db, "m")
	if len(got) != len(want) {
		t.Fatalf("scan found %d documents, model has %d", len(got), len(want))
	}
	for _, d := range got {
		if !reflect.DeepEqual(d, want[d["_id"].(string)]) {
			t.Fatalf("document %v differs from model", d["_id"])
		}
	}
	if n, _ := db.Count("m"); n != int64(len(want)) {
		t.Fatalf("Count = %d, model has %d", n, len(want))
	}
	checkPages(t, db)
}

func BenchmarkInsertMany1000(b *testing.B) {
	db, _ := openDB(b)
	batch := make([]map[string]any, 1000)
	for b.Loop() {
		for i := range batch {
			batch[i] = map[string]any{"name": fmt.Sprintf("user %d", i), "age": int64(i % 90), "tags": []any{"a", "b"}}
		}
		if _, err := db.InsertMany("users", batch); err != nil {
			b.Fatal(err)
		}
	}
}

// TestMillionDocuments stores 1,000,000 documents,
// read them all back, and pass the page check. Slow, so opt-in:
// BURROWDB_MILLION=1 go test -run TestMillionDocuments -v ./internal/doc
func TestMillionDocuments(t *testing.T) {
	if os.Getenv("BURROWDB_MILLION") == "" {
		t.Skip("set BURROWDB_MILLION=1 to run")
	}
	db, path := openDB(t)
	const total, batchSize = 1_000_000, 5000
	start := time.Now()
	batch := make([]map[string]any, batchSize)
	for done := 0; done < total; done += batchSize {
		for i := range batch {
			n := done + i
			batch[i] = map[string]any{"n": int64(n), "name": fmt.Sprintf("user %d", n), "age": int64(n % 90)}
		}
		if _, err := db.InsertMany("users", batch); err != nil {
			t.Fatal(err)
		}
	}
	insert := time.Since(start)

	db = reopen(t, db, path)
	start = time.Now()
	seen := make([]bool, total)
	count := 0
	err := db.Scan("users", func(d map[string]any) bool {
		seen[d["n"].(int64)] = true
		count++
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	scan := time.Since(start)
	if count != total || slices.Contains(seen, false) {
		t.Fatalf("scan returned %d documents, want %d, each once", count, total)
	}
	if n, _ := db.Count("users"); n != total {
		t.Fatalf("Count = %d", n)
	}
	start = time.Now()
	checkPages(t, db)
	info, _ := os.Stat(path)
	t.Logf("inserted %d docs in %v (%.0f/s), scanned in %v (%.0f/s), check %v, file %.0f MB",
		total, insert.Round(time.Millisecond), total/insert.Seconds(), scan.Round(time.Millisecond),
		total/scan.Seconds(), time.Since(start).Round(time.Millisecond), float64(info.Size())/1e6)
}
