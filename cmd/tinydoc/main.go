// Command tinydoc is the command-line entry point. For now it exposes the
// phase 1 key-value store; later phases add `serve` and `shell`.
package main

import (
	"fmt"
	"os"

	"github.com/Aliraza777/tinydoc/internal/kv"
)

const version = "0.0.1"

const usage = `tinydoc: a document database built from scratch

usage:
  tinydoc version
  tinydoc kv <file> set <key> <value>
  tinydoc kv <file> get <key>
  tinydoc kv <file> del <key>
  tinydoc kv <file> keys
  tinydoc kv <file> stats
  tinydoc kv <file> compact
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
