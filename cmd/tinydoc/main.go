// Command tinydoc is the command-line entry point. It exposes the storage
// engines built so far; later phases add `serve` and `shell`.
package main

import (
	"bytes"
	"fmt"
	"os"

	"github.com/Aliraza777/tinydoc/internal/btree"
	"github.com/Aliraza777/tinydoc/internal/kv"
	"github.com/Aliraza777/tinydoc/internal/pager"
)

const version = "0.0.2"

const usage = `tinydoc: a document database built from scratch

usage:
  tinydoc version

  B+tree storage engine (phase 2):
  tinydoc db <file> set <key> <value>
  tinydoc db <file> get <key>
  tinydoc db <file> del <key>
  tinydoc db <file> scan [from] [to]   keys in [from, to), in order
  tinydoc db <file> stats
  tinydoc db <file> check              verify the tree and page accounting

  Append-only log store (phase 1):
  tinydoc kv <file> set|get|del|keys|stats|compact ...
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
	default:
		fmt.Print(usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func runKV(args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: tinydoc kv <file> <set|get|del|keys|stats|compact> ...")
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
		return fmt.Errorf("usage: tinydoc db <file> <set|get|del|scan|stats|check> ...")
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
