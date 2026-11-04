// Package kv is tinydoc's first storage engine: a key-value store on an
// append-only log, in the style of Bitcask.
//
// Every Set or Delete appends a checksummed record to one file. An in-memory
// map from key to the value's file offset is rebuilt on Open by replaying the
// log. Replay stops at the first invalid record and truncates the file there,
// which is what a crash in the middle of an append leaves behind.
package kv

import (
	"bufio"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

// ErrClosed is returned by operations on a closed store.
var ErrClosed = errors.New("kv: store is closed")

// Options configure a Store.
type Options struct {
	// NoSync skips fsync after each write. Faster, but the last writes can be
	// lost on power failure (not on a process crash).
	NoSync bool
}

// Stats describe the log.
type Stats struct {
	Keys      int
	FileSize  int64
	DeadBytes int64 // bytes held by overwritten or deleted records; reclaimed by Compact
	// Recovered is how many bytes of a torn or corrupt tail were cut off on Open.
	Recovered int64
}

type entry struct {
	valOff int64  // offset of the value bytes
	valLen uint32 // length of the value
	recLen int64  // full record length, for dead-byte accounting
}

// Store is safe for concurrent use: one writer at a time, many readers.
type Store struct {
	mu        sync.RWMutex
	path      string
	f         *os.File
	opts      Options
	index     map[string]entry
	size      int64
	dead      int64
	recovered int64
}

// Open opens or creates the log at path.
func Open(path string, opts Options) (*Store, error) {
	// A leftover from a compaction interrupted before its rename is never
	// the live log, so it is safe to discard.
	_ = os.Remove(path + ".compact")

	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	s := &Store{path: path, f: f, opts: opts}
	if err := s.load(); err != nil {
		f.Close()
		return nil, err
	}
	return s, nil
}

// load replays the log, builds the index, and truncates any invalid tail.
func (s *Store) load() error {
	info, err := s.f.Stat()
	if err != nil {
		return err
	}
	fileSize := info.Size()
	s.index = make(map[string]entry)
	s.size, s.dead = 0, 0

	r := bufio.NewReaderSize(io.NewSectionReader(s.f, 0, fileSize), 1<<16)
	hdr := make([]byte, headerSize)
	var off int64
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			break // clean end, or a partial header
		}
		h := decodeHeader(hdr)
		if !h.plausible() {
			break
		}
		body := make([]byte, int(h.keyLen)+int(h.valLen))
		if _, err := io.ReadFull(r, body); err != nil {
			break
		}
		crc := crc32.Update(crc32.Checksum(hdr[4:], crcTable), crcTable, body)
		if crc != h.crc {
			break
		}
		recLen := int64(headerSize + len(body))
		key := string(body[:h.keyLen])
		if old, ok := s.index[key]; ok {
			s.dead += old.recLen
		}
		if h.kind == kindDelete {
			delete(s.index, key)
			s.dead += recLen // a tombstone is itself dead once applied
		} else {
			s.index[key] = entry{valOff: off + headerSize + int64(h.keyLen), valLen: h.valLen, recLen: recLen}
		}
		off += recLen
	}

	s.size = off
	if off < fileSize {
		s.recovered = fileSize - off
		if err := s.f.Truncate(off); err != nil {
			return fmt.Errorf("kv: truncating invalid tail: %w", err)
		}
		if err := s.f.Sync(); err != nil {
			return err
		}
	}
	return nil
}

// Get returns the value for key, and false if it doesn't exist.
func (s *Store) Get(key []byte) ([]byte, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.f == nil {
		return nil, false, ErrClosed
	}
	e, ok := s.index[string(key)]
	if !ok {
		return nil, false, nil
	}
	val := make([]byte, e.valLen)
	if _, err := s.f.ReadAt(val, e.valOff); err != nil {
		return nil, false, err
	}
	return val, true, nil
}

// Set stores value under key. When it returns nil the write is on disk
// (unless Options.NoSync).
func (s *Store) Set(key, value []byte) error {
	if err := checkKV(key, value); err != nil {
		return err
	}
	return s.append(kindPut, key, value)
}

// Delete removes key. Deleting a missing key is not an error.
func (s *Store) Delete(key []byte) error {
	if err := checkKV(key, nil); err != nil {
		return err
	}
	s.mu.RLock()
	_, exists := s.index[string(key)]
	s.mu.RUnlock()
	if !exists {
		return nil
	}
	return s.append(kindDelete, key, nil)
}

func (s *Store) append(kind byte, key, value []byte) error {
	rec := encodeRecord(kind, key, value)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return ErrClosed
	}
	if _, err := s.f.WriteAt(rec, s.size); err != nil {
		// Drop whatever part of the record reached the file, so the next
		// append doesn't land behind garbage.
		_ = s.f.Truncate(s.size)
		return err
	}
	if !s.opts.NoSync {
		if err := s.f.Sync(); err != nil {
			return err
		}
	}
	k := string(key)
	if old, ok := s.index[k]; ok {
		s.dead += old.recLen
	}
	recLen := int64(len(rec))
	if kind == kindDelete {
		delete(s.index, k)
		s.dead += recLen
	} else {
		s.index[k] = entry{valOff: s.size + headerSize + int64(len(key)), valLen: uint32(len(value)), recLen: recLen}
	}
	s.size += recLen
	return nil
}

// Keys returns all keys in sorted order.
func (s *Store) Keys() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	keys := make([]string, 0, len(s.index))
	for k := range s.index {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// Stats reports the log's size and how much of it is reclaimable.
func (s *Store) Stats() Stats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Stats{Keys: len(s.index), FileSize: s.size, DeadBytes: s.dead, Recovered: s.recovered}
}

// Compact rewrites the log with only live records. It writes a new file,
// syncs it, and renames it over the old one, so a crash at any point leaves
// either the old log or the new one, never a mix.
func (s *Store) Compact() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return ErrClosed
	}
	tmpPath := s.path + ".compact"
	tmp, err := os.OpenFile(tmpPath, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmpPath)
		}
	}()

	w := bufio.NewWriterSize(tmp, 1<<16)
	index := make(map[string]entry, len(s.index))
	var off int64
	for k, e := range s.index {
		val := make([]byte, e.valLen)
		if _, err := s.f.ReadAt(val, e.valOff); err != nil {
			return err
		}
		rec := encodeRecord(kindPut, []byte(k), val)
		if _, err := w.Write(rec); err != nil {
			return err
		}
		index[k] = entry{valOff: off + headerSize + int64(len(k)), valLen: e.valLen, recLen: int64(len(rec))}
		off += int64(len(rec))
	}
	if err := w.Flush(); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, s.path); err != nil {
		return err
	}
	if err := syncDir(filepath.Dir(s.path)); err != nil {
		return err
	}
	ok = true
	s.f.Close()
	s.f, s.index, s.size, s.dead = tmp, index, off, 0
	return nil
}

// Close closes the store. Further operations return ErrClosed.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.f == nil {
		return nil
	}
	err := s.f.Close()
	s.f = nil
	return err
}

// syncDir makes a rename durable by syncing the directory entry.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
