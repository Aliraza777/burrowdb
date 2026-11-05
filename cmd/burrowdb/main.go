// Command burrowdb is the burrowdb command-line tool: document commands,
// the HTTP server, and low-level access to the storage engines.
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Aliraza777/burrowdb/internal/btree"
	"github.com/Aliraza777/burrowdb/internal/doc"
	"github.com/Aliraza777/burrowdb/internal/kv"
	"github.com/Aliraza777/burrowdb/internal/pager"
	"github.com/Aliraza777/burrowdb/internal/query"
	"github.com/Aliraza777/burrowdb/internal/server"
)

const version = "0.1.0"

const usage = `burrowdb: a document database built from scratch

usage:
  burrowdb version

  HTTP server:
  burrowdb serve --data <file> [--addr 127.0.0.1:7070] [--token <secret>]
      The token can also come from $BURROWDB_TOKEN. Clients send it as
      "Authorization: Bearer <token>".

  Documents:
  burrowdb doc <file> insert <collection> '<json>'   prints the new _id
  burrowdb doc <file> get <collection> <_id>
  burrowdb doc <file> replace <collection> <_id> '<json>'
  burrowdb doc <file> delete <collection> <_id>
  burrowdb doc <file> find <collection> [limit]      every document, in _id order
  burrowdb doc <file> query <collection> '<query>'    e.g. '{"filter":{"age":{"$gt":30}},"sort":{"age":-1},"limit":10}'
  burrowdb doc <file> count <collection> ['<filter>']
  burrowdb doc <file> collections
  burrowdb doc <file> import <collection> <file.jsonl>   one JSON document per line
  burrowdb doc <file> index <collection> <field>     create an index (dotted paths work)
  burrowdb doc <file> dropindex <collection> <field>
  burrowdb doc <file> indexes <collection>
  burrowdb doc <file> explain <collection> '<query>' show the plan and the work it did
  burrowdb doc <file> check                          verify pages and every index
  burrowdb doc <file> drop <collection>

  B+tree storage engine (low level):
  burrowdb db <file> set <key> <value>
  burrowdb db <file> get <key>
  burrowdb db <file> del <key>
  burrowdb db <file> scan [from] [to]   keys in [from, to), in order
  burrowdb db <file> stats
  burrowdb db <file> check              verify the tree and page accounting

  Append-only log store (low level):
  burrowdb kv <file> set|get|del|keys|stats|compact ...
`

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return nil
	}
	switch args[0] {
	case "version":
		fmt.Println(version)
		return nil
	case "kv":
		return runKV(args[1:])
	case "db":
		return runDB(args[1:])
	case "doc":
		return runDoc(args[1:])
	case "serve":
		return runServe(args[1:])
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runKV(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: burrowdb kv <file> <set|get|del|keys|stats|compact> ...")
	}
	s, err := kv.Open(args[0], kv.Options{})
	if err != nil {
		return err
	}
	defer s.Close()
	if st := s.Stats(); st.Recovered > 0 {
		fmt.Fprintf(os.Stderr, "recovered: cut %d bytes of an incomplete write from the end of the log\n", st.Recovered)
	}

	op, rest := args[1], args[2:]
	need := map[string]int{"set": 2, "get": 1, "del": 1, "keys": 0, "stats": 0, "compact": 0}
	n, ok := need[op]
	if !ok {
		return fmt.Errorf("unknown kv command %q", op)
	}
	if len(rest) != n {
		return fmt.Errorf("%s takes %d argument(s)", op, n)
	}

	switch op {
	case "set":
		return s.Set([]byte(rest[0]), []byte(rest[1]))
	case "get":
		v, found, err := s.Get([]byte(rest[0]))
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("key %q not found", rest[0])
		}
		fmt.Println(string(v))
	case "del":
		return s.Delete([]byte(rest[0]))
	case "keys":
		for _, k := range s.Keys() {
			fmt.Println(k)
		}
	case "stats":
		st := s.Stats()
		fmt.Printf("keys: %d\nfile size: %d bytes\nreclaimable: %d bytes\n", st.Keys, st.FileSize, st.DeadBytes)
	case "compact":
		before := s.Stats().FileSize
		if err := s.Compact(); err != nil {
			return err
		}
		fmt.Printf("compacted: %d -> %d bytes\n", before, s.Stats().FileSize)
	}
	return nil
}

func runDB(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: burrowdb db <file> <set|get|del|scan|stats|check> ...")
	}
	db, err := pager.Open(args[0])
	if err != nil {
		return err
	}
	defer db.Close()

	op, rest := args[1], args[2:]
	switch op {
	case "set":
		if len(rest) != 2 {
			return fmt.Errorf("set takes <key> <value>")
		}
		return db.Set([]byte(rest[0]), []byte(rest[1]))
	case "get":
		if len(rest) != 1 {
			return fmt.Errorf("get takes <key>")
		}
		v, found, err := db.Get([]byte(rest[0]))
		if err != nil {
			return err
		}
		if !found {
			return fmt.Errorf("key %q not found", rest[0])
		}
		fmt.Println(string(v))
	case "del":
		if len(rest) != 1 {
			return fmt.Errorf("del takes <key>")
		}
		existed, err := db.Delete([]byte(rest[0]))
		if err == nil && !existed {
			fmt.Fprintf(os.Stderr, "key %q did not exist\n", rest[0])
		}
		return err
	case "scan":
		if len(rest) > 2 {
			return fmt.Errorf("scan takes [from] [to]")
		}
		var from, to []byte
		if len(rest) > 0 {
			from = []byte(rest[0])
		}
		if len(rest) > 1 {
			to = []byte(rest[1])
		}
		return db.View(func(t *btree.Tree) error {
			for it := t.Seek(from); it.Valid(); it.Next() {
				if to != nil && bytes.Compare(it.Key(), to) >= 0 {
					break
				}
				fmt.Printf("%s = %s\n", it.Key(), it.Value())
			}
			return nil
		})
	case "stats":
		st := db.Stats()
		fmt.Printf("commits: %d\npages: %d (%d bytes)\nfree pages: %d\n",
			st.TxID, st.Pages, st.Pages*btree.PageSize, st.FreePages)
	case "check":
		if err := db.Check(); err != nil {
			return err
		}
		fmt.Println("ok: tree structure valid, every page accounted for")
	default:
		return fmt.Errorf("unknown db command %q", op)
	}
	return nil
}

func runDoc(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: burrowdb doc <file> <insert|get|replace|delete|find|count|collections|drop> ...")
	}
	db, err := doc.Open(args[0])
	if err != nil {
		return err
	}
	defer db.Close()

	op, rest := args[1], args[2:]
	need := map[string][2]int{ // min, max arguments
		"insert": {2, 2}, "get": {2, 2}, "replace": {3, 3}, "delete": {2, 2},
		"find": {1, 2}, "count": {1, 2}, "collections": {0, 0}, "drop": {1, 1},
		"query": {2, 2}, "index": {2, 2}, "dropindex": {2, 2}, "indexes": {1, 1},
		"explain": {2, 2}, "check": {0, 0}, "import": {2, 2},
	}
	n, ok := need[op]
	if !ok {
		return fmt.Errorf("unknown doc command %q", op)
	}
	if len(rest) < n[0] || len(rest) > n[1] {
		return fmt.Errorf("wrong number of arguments for %s (see `burrowdb` for usage)", op)
	}

	printDoc := func(d map[string]any) error {
		out, err := doc.ToJSON(d)
		if err != nil {
			return err
		}
		fmt.Println(string(out))
		return nil
	}
	switch op {
	case "insert":
		d, err := doc.FromJSON([]byte(rest[1]))
		if err != nil {
			return err
		}
		id, err := db.Insert(rest[0], d)
		if err != nil {
			return err
		}
		fmt.Println(id)
	case "get":
		d, err := db.Get(rest[0], rest[1])
		if err != nil {
			return err
		}
		return printDoc(d)
	case "replace":
		d, err := doc.FromJSON([]byte(rest[2]))
		if err != nil {
			return err
		}
		return db.Replace(rest[0], rest[1], d)
	case "delete":
		deleted, err := db.Delete(rest[0], rest[1])
		if err == nil && !deleted {
			return doc.ErrNotFound
		}
		return err
	case "find":
		limit := -1
		if len(rest) == 2 {
			if _, err := fmt.Sscan(rest[1], &limit); err != nil || limit < 0 {
				return fmt.Errorf("limit must be a non-negative number")
			}
		}
		var printErr error
		err := db.Scan(rest[0], func(d map[string]any) bool {
			if limit == 0 {
				return false
			}
			limit--
			printErr = printDoc(d)
			return printErr == nil
		})
		if err != nil {
			return err
		}
		return printErr
	case "query":
		q, err := query.ParseQueryJSON([]byte(rest[1]))
		if err != nil {
			return err
		}
		docs, err := query.Find(db, rest[0], q)
		if err != nil {
			return err
		}
		for _, d := range docs {
			if err := printDoc(d); err != nil {
				return err
			}
		}
	case "import":
		return importJSONL(db, rest[0], rest[1])
	case "index":
		return db.CreateIndex(rest[0], rest[1])
	case "dropindex":
		return db.DropIndex(rest[0], rest[1])
	case "indexes":
		idx, err := db.Indexes(rest[0])
		if err != nil {
			return err
		}
		for _, i := range idx {
			kind := ""
			if i.Multikey {
				kind = " (multikey)"
			}
			fmt.Println(i.Field + kind)
		}
	case "explain":
		q, err := query.ParseQueryJSON([]byte(rest[1]))
		if err != nil {
			return err
		}
		start := time.Now()
		ex, err := query.ExplainQuery(db, rest[0], q)
		if err != nil {
			return err
		}
		out, _ := json.MarshalIndent(struct {
			query.Explain
			Millis float64 `json:"millis"`
		}{ex, float64(time.Since(start).Microseconds()) / 1000}, "", "  ")
		fmt.Println(string(out))
	case "check":
		if err := db.Pager().Check(); err != nil {
			return err
		}
		if err := db.CheckIndexes(); err != nil {
			return err
		}
		fmt.Println("ok: pages accounted for, every index matches its documents")
	case "count":
		if len(rest) == 2 {
			m, err := doc.FromJSON([]byte(rest[1]))
			if err != nil {
				return err
			}
			f, err := query.ParseFilter(m)
			if err != nil {
				return err
			}
			c, err := query.Count(db, rest[0], f)
			if err != nil {
				return err
			}
			fmt.Println(c)
			return nil
		}
		c, err := db.Count(rest[0])
		if err != nil {
			return err
		}
		fmt.Println(c)
	case "collections":
		names, err := db.Collections()
		if err != nil {
			return err
		}
		for _, name := range names {
			fmt.Println(name)
		}
	case "drop":
		existed, err := db.DropCollection(rest[0])
		if err == nil && !existed {
			return fmt.Errorf("collection %q does not exist", rest[0])
		}
		return err
	}
	return nil
}

// importJSONL inserts one JSON document per line, in batches of 1,000.
func importJSONL(db *doc.DB, coll, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), doc.MaxDocSize+1<<20)
	var batch []map[string]any
	total, line := 0, 0
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		if _, err := db.InsertMany(coll, batch); err != nil {
			return fmt.Errorf("batch ending at line %d: %w", line, err)
		}
		total += len(batch)
		batch = batch[:0]
		return nil
	}
	for sc.Scan() {
		line++
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		d, err := doc.FromJSON(sc.Bytes())
		if err != nil {
			return fmt.Errorf("line %d: %w", line, err)
		}
		batch = append(batch, d)
		if len(batch) == 1000 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	fmt.Printf("imported %d documents into %s\n", total, coll)
	return nil
}

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	data := fs.String("data", "", "database file (created if missing)")
	addr := fs.String("addr", "127.0.0.1:7070", "address to listen on")
	token := fs.String("token", os.Getenv("BURROWDB_TOKEN"), "require this bearer token")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *data == "" {
		return fmt.Errorf("serve needs --data <file>")
	}
	db, err := doc.Open(*data)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Addr:              *addr,
		Handler:           server.Handler(db, server.Options{Token: *token, Version: version}),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       2 * time.Minute,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       2 * time.Minute,
	}
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		db.Close()
		return err
	}
	auth := "no token: anyone who can reach this address has full access"
	if *token != "" {
		auth = "bearer token required"
	}
	fmt.Fprintf(os.Stderr, "burrowdb %s serving %s on http://%s (%s)\n", version, *data, ln.Addr(), auth)

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	select {
	case err := <-serveErr:
		db.Close()
		return err
	case <-stop:
	}
	fmt.Fprintln(os.Stderr, "shutting down: finishing requests in flight")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	shutdownErr := srv.Shutdown(ctx)
	if err := db.Close(); err != nil {
		return err
	}
	return shutdownErr
}
