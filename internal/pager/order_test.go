package pager

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Aliraza777/burrowdb/internal/btree"
)

// recordingFile logs every write and sync, so a test can check that a commit
// reaches the disk in a crash-safe order even on power loss, which a killed
// process can't reveal (the OS still holds its unsynced writes).
type recordingFile struct {
	*os.File
	ops []string
}

func (r *recordingFile) WriteAt(b []byte, off int64) (int, error) {
	if off < 2*pageSize {
		r.ops = append(r.ops, "meta")
	} else {
		r.ops = append(r.ops, "page")
	}
	return r.File.WriteAt(b, off)
}

func (r *recordingFile) Sync() error {
	r.ops = append(r.ops, "sync")
	return r.File.Sync()
}

// A commit must write all pages, sync, write the meta page, and sync again.
// Without the first sync the disk may persist the meta page before the pages
// it points to; without the second, a returned commit may not be durable.
func TestCommitOrder(t *testing.T) {
	f, err := os.OpenFile(filepath.Join(t.TempDir(), "db"), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	rec := &recordingFile{File: f}
	db, err := openFile(rec)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for round := range 3 {
		rec.ops = nil
		err := db.Update(func(tr *btree.Tree) error {
			for i := range 200 {
				if err := tr.Set(key(round*1000+i), make([]byte, 300)); err != nil {
					return err
				}
			}
			if round > 0 {
				for i := range 100 {
					tr.Delete(key((round-1)*1000 + i))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		// Collapse runs of page writes, then compare with the safe sequence.
		var got []string
		for _, op := range rec.ops {
			if op == "page" && len(got) > 0 && got[len(got)-1] == "page" {
				continue
			}
			got = append(got, op)
		}
		if seq := strings.Join(got, " "); seq != "page sync meta sync" {
			t.Fatalf("round %d: commit did %q, want \"page sync meta sync\"", round, seq)
		}
	}
}
