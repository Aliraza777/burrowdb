// Command burrowdb is the command-line entry point. It exposes the storage
// engines built so far; later phases add `serve` and `shell`.
package main

import (
	"bytes"
	"fmt"
	"os"

	"github.com/Aliraza777/burrowdb/internal/btree"
	"github.com/Aliraza777/burrowdb/internal/doc"
	"github.com/Aliraza777/burrowdb/internal/kv"
	"github.com/Aliraza777/burrowdb/internal/pager"
	"github.com/Aliraza777/burrowdb/internal/query"
)

const version = "0.0.4"

const usage = `burrowdb: a document database built from scratch

usage:
  burrowdb version

  Documents (phase 3):
  burrowdb doc <file> insert <collection> '<json>'   prints the new _id
  burrowdb doc <file> get <collection> <_id>
  burrowdb doc <file> replace <collection> <_id> '<json>'
  burrowdb doc <file> delete <collection> <_id>
  burrowdb doc <file> find <collection> [limit]      every document, in _id order
  burrowdb doc <file> query <collection> '<query>'    e.g. '{"filter":{"age":{"$gt":30}},"sort":{"age":-1},"limit":10}'
  burrowdb doc <file> count <collection> ['<filter>']
  burrowdb doc <file> collections
  burrowdb doc <file> drop <collection>

  B+tree storage engine (phase 2):
  burrowdb db <file> set <key> <value>
  burrowdb db <file> get <key>
  burrowdb db <file> del <key>
  burrowdb db <file> scan [from] [to]   keys in [from, to), in order
  burrowdb db <file> stats
  burrowdb db <file> check              verify the tree and page accounting

  Append-only log store (phase 1):
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
		"query": {2, 2},
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
