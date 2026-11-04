# tinydoc

A MongoDB-style document database, built from scratch in Go to learn how databases really work: storage on disk, crash recovery, B+trees, query engines and indexes. See [PLAN.md](PLAN.md) for the full roadmap.

## Status

| Phase | | |
| --- | --- | --- |
| 0 | Project setup, CI | ✅ |
| 1 | Key-value store on an append-only log | ✅ |
| 2 | B+tree storage engine | ✅ |
| 3 | Documents and collections | next |
| 4–8 | Queries, indexes, transactions, server, client | planned |

## Phase 2: a copy-on-write B+tree on disk

`internal/btree` is a B+tree of 4 KB pages with get, set, delete (merging under-full pages) and ordered range scans. It never changes a page in place: an update writes new copies of the pages from the root down to the changed leaf.

`internal/pager` stores the tree in one file. Two alternating meta pages hold the root, a transaction id and a checksum. A commit writes the new pages to places no committed tree uses, syncs, then writes the meta page and syncs again. Until the meta page lands, the old one still describes an untouched tree, so a crash at any point loses at most the commit in progress. Freed pages go on a free-list and are reused, so the file stops growing once it reaches its working size.

```sh
./tinydoc db demo.db set apple red
./tinydoc db demo.db set banana yellow
./tinydoc db demo.db scan            # all keys, in order
./tinydoc db demo.db scan a b        # keys in [a, b)
./tinydoc db demo.db check           # verify structure and page accounting
```

### How it's tested

- **Fuzzing against a model:** random sequences of sets and deletes run against the tree and a plain Go map. After each, the contents and order must match, the structure must be valid (sorted keys, balanced depth, nodes within a page), and every page must be accounted for. Hundreds of thousands of executions per minute.
- **Crash test:** a process committing 10-key batches is killed with `SIGKILL` 500 times. After each kill the file opens, passes the full check, and holds whole batches only.
- **Commit order:** a recording file checks that each commit does `write pages → sync → write meta → sync`, which a crash test can't see because the OS keeps a killed process's writes.
- **Torn meta page, interrupted commit, trailing junk, corrupt file:** each recovers to the last good commit or is refused.
- **Mutation checks:** planting known bugs (reusing pages too early, a single meta slot, a missing sync, broken merging, an off-by-one in search) makes the tests fail.

### Benchmarks (Apple M1 Pro)

| | |
| --- | --- |
| B+tree in memory: set / get | 6.1 µs / 0.64 µs |
| On disk: one set per commit | 9.3 ms (each commit waits for a full flush to storage) |
| On disk: 1,000 sets per commit | 22 ms, 22 µs per key |
| On disk: get | 3.8 µs (no page cache yet) |

Per-commit cost is dominated by `fsync`, which is why batching writes into transactions (phase 6) matters.

## Phase 1: an append-only key-value store

Every write is appended to one log file as a checksummed record:

```
crc32 | keyLen | valLen | kind | key | value
```

An in-memory map points each key at its value's offset in the file, and it is rebuilt on startup by replaying the log. If the process died in the middle of a write, the last record fails its checksum (or is cut short). Replay stops there and trims it off, so the store always opens in a consistent state. `Compact` rewrites the log with only live records into a new file and atomically renames it into place.

Try it:

```sh
go build ./cmd/tinydoc
./tinydoc kv demo.log set greeting hello
./tinydoc kv demo.log get greeting
./tinydoc kv demo.log stats
```

### How it's tested

- **Crash test:** the test re-runs itself as a writer process, kills it with `SIGKILL` at random moments, reopens the store and checks every acknowledged write. It passes 1,000 rounds in a row (`TINYDOC_CRASH_ITERS=1000 go test -run TestCrashRecovery ./internal/kv`).
- **Torn writes:** the file is cut at every byte inside the last record, and each time recovery must keep all earlier records and accept new writes.
- **Corruption:** a flipped bit or trailing garbage is detected by the checksum and cut off.
- **Concurrency:** readers, writers and compaction run in parallel under `go test -race`.

```sh
go test -race ./...
```

## License

MIT
