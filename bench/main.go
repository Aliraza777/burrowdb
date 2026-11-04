// Command bench runs the same document workloads against burrowdb and
// SQLite (used as a document store: a JSON column with expression indexes)
// and prints a Markdown table.
//
//	cd bench && go run . -n 100000
//
// Both are embedded and durable on every commit. SQLite runs in WAL mode
// with synchronous=FULL and fullfsync=ON, matching burrowdb, which on macOS
// syncs with F_FULLFSYNC. Every result is decoded into a Go map for both.
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Aliraza777/burrowdb/internal/doc"
	"github.com/Aliraza777/burrowdb/internal/query"
	_ "modernc.org/sqlite"
)

var cities = []string{"Lahore", "Karachi", "Multan", "Quetta", "Peshawar"}

func makeDoc(i int) map[string]any {
	return map[string]any{
		"n":    int64(i),
		"age":  int64(i % 90),
		"city": cities[i%len(cities)],
		"name": fmt.Sprintf("user %d", i),
		"tags": []any{"t" + fmt.Sprint(i%7), "t" + fmt.Sprint(i%11)},
	}
}

// store is one database under test.
type store interface {
	name() string
	insertBatch(docs []map[string]any) error
	insertOne(d map[string]any) error
	findN(n int64) (int, error)             // documents with n == value
	findAgeRange(lo, hi int64) (int, error) // documents with lo <= age < hi
	countCity(city string) (int, error)     // unindexed filter, full scan
	size() (int64, error)
	close() error
}

// --- burrowdb ---

type burrow struct {
	db   *doc.DB
	path string
}

func openBurrow(dir string) (*burrow, error) {
	path := filepath.Join(dir, "bench.burrow")
	db, err := doc.Open(path)
	if err != nil {
		return nil, err
	}
	for _, f := range []string{"n", "age"} {
		if err := db.CreateIndex("users", f); err != nil {
			return nil, err
		}
	}
	return &burrow{db, path}, nil
}

func (b *burrow) name() string { return "burrowdb" }
func (b *burrow) insertBatch(docs []map[string]any) error {
	_, err := b.db.InsertMany("users", docs)
	return err
}
func (b *burrow) insertOne(d map[string]any) error { _, err := b.db.Insert("users", d); return err }
func (b *burrow) find(filter string) (int, error) {
	q, err := query.ParseQueryJSON([]byte(`{"filter":` + filter + `}`))
	if err != nil {
		return 0, err
	}
	docs, err := query.Find(b.db, "users", q)
	return len(docs), err
}
func (b *burrow) findN(n int64) (int, error) { return b.find(fmt.Sprintf(`{"n":%d}`, n)) }
func (b *burrow) findAgeRange(lo, hi int64) (int, error) {
	return b.find(fmt.Sprintf(`{"age":{"$gte":%d,"$lt":%d}}`, lo, hi))
}
func (b *burrow) countCity(city string) (int, error) {
	q, _ := query.ParseQueryJSON([]byte(fmt.Sprintf(`{"filter":{"city":%q}}`, city)))
	return query.Count(b.db, "users", q.Filter)
}
func (b *burrow) size() (int64, error) { return fileSize(b.path) }
func (b *burrow) close() error         { return b.db.Close() }

// --- SQLite ---

type lite struct {
	db   *sql.DB
	path string
	id   int
	mu   sync.Mutex
}

func openSQLite(dir string) (*lite, error) {
	path := filepath.Join(dir, "bench.sqlite")
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=fullfsync(1)&_pragma=checkpoint_fullfsync(1)&_pragma=busy_timeout(30000)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	for _, stmt := range []string{
		`CREATE TABLE users (id TEXT PRIMARY KEY, body TEXT NOT NULL)`,
		`CREATE INDEX users_n ON users (json_extract(body, '$.n'))`,
		`CREATE INDEX users_age ON users (json_extract(body, '$.age'))`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			return nil, err
		}
	}
	return &lite{db: db, path: path}, nil
}

func (l *lite) nextID() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.id++
	return fmt.Sprintf("%024x", l.id)
}

func (l *lite) name() string { return "SQLite (JSON column)" }

func (l *lite) insertBatch(docs []map[string]any) error {
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO users (id, body) VALUES (?, ?)`)
	if err != nil {
		tx.Rollback()
		return err
	}
	for _, d := range docs {
		body, _ := json.Marshal(d)
		if _, err := stmt.Exec(l.nextID(), string(body)); err != nil {
			tx.Rollback()
			return err
		}
	}
	stmt.Close()
	return tx.Commit()
}

func (l *lite) insertOne(d map[string]any) error {
	body, _ := json.Marshal(d)
	_, err := l.db.Exec(`INSERT INTO users (id, body) VALUES (?, ?)`, l.nextID(), string(body))
	return err
}

// rows decodes every result into a map, as burrowdb does.
func (l *lite) rows(query string, args ...any) (int, error) {
	rows, err := l.db.Query(query, args...)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var body string
		if err := rows.Scan(&body); err != nil {
			return n, err
		}
		var d map[string]any
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

func (l *lite) findN(n int64) (int, error) {
	return l.rows(`SELECT body FROM users WHERE json_extract(body, '$.n') = ?`, n)
}
func (l *lite) findAgeRange(lo, hi int64) (int, error) {
	return l.rows(`SELECT body FROM users WHERE json_extract(body, '$.age') >= ? AND json_extract(body, '$.age') < ?`, lo, hi)
}
func (l *lite) countCity(city string) (int, error) {
	var n int
	err := l.db.QueryRow(`SELECT count(*) FROM users WHERE json_extract(body, '$.city') = ?`, city).Scan(&n)
	return n, err
}
func (l *lite) size() (int64, error) {
	l.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`)
	a, err := fileSize(l.path)
	b, _ := fileSize(l.path + "-wal")
	return a + b, err
}
func (l *lite) close() error { return l.db.Close() }

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// --- workloads ---

type result struct {
	workload string
	values   map[string]string
}

func timed(fn func() error) (time.Duration, error) {
	start := time.Now()
	err := fn()
	return time.Since(start), err
}

func rate(n int, d time.Duration) string { return fmt.Sprintf("%s/s", human(float64(n)/d.Seconds())) }

func human(v float64) string {
	switch {
	case v >= 1e6:
		return fmt.Sprintf("%.2fM", v/1e6)
	case v >= 1e3:
		return fmt.Sprintf("%.1fk", v/1e3)
	}
	return fmt.Sprintf("%.0f", v)
}

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "bench:", err)
		os.Exit(1)
	}
}

func run(s store, n int, results map[string]map[string]string, order *[]string) {
	put := func(workload, v string) {
		if results[workload] == nil {
			results[workload] = map[string]string{}
			*order = append(*order, workload)
		}
		results[workload][s.name()] = v
		fmt.Fprintf(os.Stderr, "  %-28s %-22s %s\n", s.name(), workload, v)
	}
	r := rand.New(rand.NewPCG(1, 2))

	const batch = 1000
	d, err := timed(func() error {
		for i := 0; i < n; i += batch {
			docs := make([]map[string]any, 0, batch)
			for j := i; j < min(n, i+batch); j++ {
				docs = append(docs, makeDoc(j))
			}
			if err := s.insertBatch(docs); err != nil {
				return err
			}
		}
		return nil
	})
	must(err)
	put(fmt.Sprintf("Bulk insert (%s docs, 1,000 per transaction)", human(float64(n))), rate(n, d))

	const singles = 200
	d, err = timed(func() error {
		for i := range singles {
			if err := s.insertOne(makeDoc(n + i)); err != nil {
				return err
			}
		}
		return nil
	})
	must(err)
	put("Single insert, one transaction each", fmt.Sprintf("%s (%.1f ms each)", rate(singles, d), float64(d.Microseconds())/1000/singles))

	const writers, each = 32, 20
	d, err = timed(func() error {
		var wg sync.WaitGroup
		errs := make(chan error, writers)
		for w := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for i := range each {
					if err := s.insertOne(makeDoc(n + singles + w*each + i)); err != nil {
						errs <- err
						return
					}
				}
			}()
		}
		wg.Wait()
		close(errs)
		return <-errs
	})
	must(err)
	put("32 concurrent writers, one insert per transaction", rate(writers*each, d))

	const lookups = 10000
	d, err = timed(func() error {
		for range lookups {
			got, err := s.findN(r.Int64N(int64(n)))
			if err != nil {
				return err
			}
			if got != 1 {
				return fmt.Errorf("%s: point lookup returned %d documents", s.name(), got)
			}
		}
		return nil
	})
	must(err)
	put("Point lookup on an indexed field", fmt.Sprintf("%s (%.0f µs each)", rate(lookups, d), float64(d.Microseconds())/lookups))

	const ranges = 50
	var matched int
	d, err = timed(func() error {
		for range ranges {
			lo := r.Int64N(88)
			got, err := s.findAgeRange(lo, lo+2)
			if err != nil {
				return err
			}
			matched = got
		}
		return nil
	})
	must(err)
	put(fmt.Sprintf("Range query on an index (~%s docs returned)", human(float64(matched))), fmt.Sprintf("%.1f ms each", float64(d.Microseconds())/1000/ranges))

	const scans = 5
	d, err = timed(func() error {
		for range scans {
			if _, err := s.countCity("Lahore"); err != nil {
				return err
			}
		}
		return nil
	})
	must(err)
	put("Count with an unindexed filter (full scan)", fmt.Sprintf("%.0f ms each", float64(d.Milliseconds())/scans))

	size, err := s.size()
	must(err)
	put("File size", fmt.Sprintf("%.1f MB", float64(size)/1e6))
}

func main() {
	n := flag.Int("n", 100_000, "documents to insert")
	flag.Parse()
	dir, err := os.MkdirTemp("", "burrowdb-bench-")
	must(err)
	defer os.RemoveAll(dir)

	results := map[string]map[string]string{}
	var order []string
	b, err := openBurrow(dir)
	must(err)
	run(b, *n, results, &order)
	must(b.close())
	l, err := openSQLite(dir)
	must(err)
	run(l, *n, results, &order)
	must(l.close())

	names := []string{b.name(), l.name()}
	fmt.Printf("| Workload | %s |\n|---|%s\n", strings.Join(names, " | "), strings.Repeat("---|", len(names)))
	for _, w := range order {
		row := []string{w}
		for _, name := range names {
			row = append(row, results[w][name])
		}
		fmt.Printf("| %s |\n", strings.Join(row, " | "))
	}
	fmt.Printf("\n%s/%s, %d CPUs, Go %s, %s documents\n", runtime.GOOS, runtime.GOARCH, runtime.NumCPU(), runtime.Version(), human(float64(*n)))
}
