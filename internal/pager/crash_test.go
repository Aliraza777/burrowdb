//go:build unix

package pager

import (
	"bufio"
	"fmt"
	"math/rand/v2"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Aliraza777/tinydoc/internal/btree"
)

const childEnv = "TINYDOC_PAGER_CRASH_CHILD"

func TestMain(m *testing.M) {
	if path := os.Getenv(childEnv); path != "" {
		crashChild(path)
		return
	}
	os.Exit(m.Run())
}

// crashChild commits batches forever: batch i sets keys i*10 .. i*10+9 and
// deletes the batch from 20 batches ago, all in one transaction. It prints i
// only after the commit returned.
func crashChild(path string) {
	db, err := Open(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	start := 0
	db.View(func(t *btree.Tree) error {
		// Resume after the highest committed batch.
		for it := t.Seek(nil); it.Valid(); it.Next() {
			if n, err := strconv.Atoi(strings.TrimPrefix(string(it.Key()), "key-")); err == nil {
				start = max(start, n/10+1)
			}
		}
		return nil
	})
	out := bufio.NewWriter(os.Stdout)
	for i := start; ; i++ {
		err := db.Update(func(t *btree.Tree) error {
			for j := range 10 {
				if err := t.Set(key(i*10+j), []byte(batchValue(i))); err != nil {
					return err
				}
			}
			if i >= 20 {
				for j := range 10 {
					t.Delete(key((i-20)*10 + j))
				}
			}
			return nil
		})
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		fmt.Fprintln(out, i)
		out.Flush()
	}
}

func batchValue(i int) string { return strings.Repeat(strconv.Itoa(i), 1+i%40) }

// Kill a committing process at random moments. After every kill the file must
// open, pass the full structural check, and hold exactly whole batches: every
// acknowledged batch (minus the ones deleted later), and never part of one.
func TestCrashRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("crash test skipped in -short mode")
	}
	iters := 25
	if v, err := strconv.Atoi(os.Getenv("TINYDOC_CRASH_ITERS")); err == nil {
		iters = v
	}
	path := filepath.Join(t.TempDir(), "db")
	acked := -1

	for round := range iters {
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(), childEnv+"="+path)
		stdout, _ := cmd.StdoutPipe()
		var stderr strings.Builder
		cmd.Stderr = &stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		lines := make(chan int, 1024)
		go func() {
			sc := bufio.NewScanner(stdout)
			for sc.Scan() {
				if n, err := strconv.Atoi(sc.Text()); err == nil {
					lines <- n
				}
			}
			close(lines)
		}()
		time.Sleep(time.Duration(5+rand.IntN(60)) * time.Millisecond)
		cmd.Process.Signal(syscall.SIGKILL)
		for n := range lines {
			acked = max(acked, n)
		}
		cmd.Wait()
		if stderr.Len() > 0 {
			t.Fatalf("round %d: child failed: %s", round, stderr.String())
		}

		db, err := Open(path)
		if err != nil {
			t.Fatalf("round %d: reopen: %v", round, err)
		}
		if err := db.Check(); err != nil {
			db.Close()
			t.Fatalf("round %d: %v", round, err)
		}
		// Count keys per batch: each batch must be all there or all gone.
		perBatch := map[int]int{}
		db.View(func(tr *btree.Tree) error {
			for it := tr.Seek(nil); it.Valid(); it.Next() {
				n, _ := strconv.Atoi(strings.TrimPrefix(string(it.Key()), "key-"))
				if string(it.Value()) != batchValue(n/10) {
					t.Errorf("round %d: key %s has the wrong value", round, it.Key())
				}
				perBatch[n/10]++
			}
			return nil
		})
		db.Close()
		top := -1
		for b, c := range perBatch {
			if c != 10 {
				t.Fatalf("round %d: batch %d is half-applied (%d of 10 keys)", round, b, c)
			}
			top = max(top, b)
		}
		// The newest batch on disk is the last acknowledged one, or the one in flight.
		if acked >= 0 && top != acked && top != acked+1 {
			t.Fatalf("round %d: newest batch on disk is %d, last acknowledged %d", round, top, acked)
		}
		// The 20 newest batches must all be there (older ones are deleted by
		// later batches, possibly by the in-flight one that did commit).
		for b := max(0, top-19); b <= top; b++ {
			if perBatch[b] != 10 {
				t.Fatalf("round %d: batch %d is missing (newest on disk %d, acknowledged %d)", round, b, top, acked)
			}
		}
		acked = max(acked, top)
	}
	t.Logf("%d crash rounds, %d batches committed, file always consistent", iters, acked+1)
}
