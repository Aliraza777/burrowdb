//go:build unix

package kv

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
)

// When this variable is set, the test binary acts as the crash test's child:
// it writes keys forever and prints each one only after Set has returned.
const childEnv = "TINYDOC_CRASH_CHILD"

func TestMain(m *testing.M) {
	if path := os.Getenv(childEnv); path != "" {
		crashChild(path)
		return
	}
	os.Exit(m.Run())
}

func crashChild(path string) {
	s, err := Open(path, Options{})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	// Redo the last key found: the previous child may have died between writing
	// it and doing its overwrite. Rewriting is idempotent.
	start := max(0, len(s.Keys())-1)
	out := bufio.NewWriter(os.Stdout)
	for i := start; ; i++ {
		key := fmt.Sprintf("key-%07d", i)
		if err := s.Set([]byte(key), []byte(valueFor(i))); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		// Every 10th write overwrites an earlier key, so replay sees updates too.
		if i%10 == 9 {
			if err := s.Set([]byte(fmt.Sprintf("key-%07d", i-5)), []byte("updated")); err != nil {
				os.Exit(2)
			}
		}
		fmt.Fprintln(out, i)
		out.Flush()
	}
}

func valueFor(i int) string { return strings.Repeat(strconv.Itoa(i), 1+i%7) }

// Kill a writer process with SIGKILL at a random moment, again and again, and
// check after each kill that every acknowledged write survived intact.
// TINYDOC_CRASH_ITERS sets the number of rounds (default 25; use 1000 for a soak run).
func TestCrashRecovery(t *testing.T) {
	if testing.Short() {
		t.Skip("crash test skipped in -short mode")
	}
	iters := 25
	if v, err := strconv.Atoi(os.Getenv("TINYDOC_CRASH_ITERS")); err == nil {
		iters = v
	}
	path := filepath.Join(t.TempDir(), "db.log")
	acked := -1 // highest key index the child confirmed

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
		time.Sleep(time.Duration(5+rand.IntN(40)) * time.Millisecond)
		cmd.Process.Signal(syscall.SIGKILL)
		for n := range lines {
			acked = max(acked, n)
		}
		cmd.Wait()
		if stderr.Len() > 0 {
			t.Fatalf("round %d: child failed: %s", round, stderr.String())
		}

		s, err := Open(path, Options{})
		if err != nil {
			t.Fatalf("round %d: reopen: %v", round, err)
		}
		for i := 0; i <= acked; i++ {
			key := fmt.Sprintf("key-%07d", i)
			got, ok, err := s.Get([]byte(key))
			// Key i (i%10 == 4) is overwritten while writing key i+5. Once i+5 is
			// acknowledged the update must be there; before that it may or may not be.
			want := valueFor(i)
			overwritable := i%10 == 4
			if overwritable && i+5 <= acked {
				want = "updated"
			}
			valid := string(got) == want || (overwritable && i+5 > acked && string(got) == "updated")
			if err != nil || !ok || !valid {
				s.Close()
				t.Fatalf("round %d: acknowledged %s lost or wrong: ok=%v got=%q want=%q err=%v",
					round, key, ok, got, want, err)
			}
		}
		// Nothing beyond one in-flight write may exist past the last acknowledged key.
		if n := len(s.Keys()); n > acked+2 {
			t.Fatalf("round %d: %d keys but only %d acknowledged", round, n, acked+1)
		}
		s.Close()
	}
	t.Logf("%d crash rounds, %d acknowledged writes all intact", iters, acked+1)
}
