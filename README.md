# tinydoc

A MongoDB-style document database, built from scratch in Go to learn how databases really work: storage on disk, crash recovery, B+trees, query engines and indexes. See [PLAN.md](PLAN.md) for the full roadmap.

## Status

| Phase | | |
| --- | --- | --- |
| 0 | Project setup, CI | ✅ |
| 1 | Key-value store on an append-only log | ✅ |
| 2 | B+tree storage engine | next |
| 3–8 | Documents, queries, indexes, transactions, server, client | planned |

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
