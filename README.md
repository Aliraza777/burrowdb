# burrowdb

A MongoDB-style document database, built from scratch in Go to learn how databases really work: storage on disk, crash recovery, B+trees, query engines and indexes. See [PLAN.md](PLAN.md) for the full roadmap.

## Status

| Phase | | |
| --- | --- | --- |
| 0 | Project setup, CI | ✅ |
| 1 | Key-value store on an append-only log | ✅ |
| 2 | B+tree storage engine | ✅ |
| 3 | Documents and collections | ✅ |
| 4 | Query engine | ✅ |
| 5 | Indexes and a query planner | next |
| 6–8 | Transactions, server, client | planned |

## Phase 4: a MongoDB-style query engine

`internal/query` runs filters, sort, skip, limit and projection over a collection:

```sh
./burrowdb doc shop.db query users '{"filter":{"age":{"$gt":30},"tags":"admin"},"sort":{"age":-1},"limit":10,"projection":{"name":1}}'
./burrowdb doc shop.db count users '{"address.city":"Lahore"}'
```

- **Operators:** `$eq $ne $gt $gte $lt $lte $in $nin $exists`, combined with `$and $or $nor`, plus plain `{"field": value}` equality.
- **MongoDB's matching rules:** dotted paths into nested objects and through arrays (`items.price`, `items.0.price`); an array matches if any element does, or as a whole; `1` equals `1.0`; range operators only compare within one type; `{"x": null}` also matches documents without `x`.
- **Sort** on several fields in the order given (kept from the JSON), with MongoDB's type order (null < numbers < strings < objects < arrays < booleans). Arrays sort by their smallest element ascending and largest descending, and ties keep `_id` order.
- **Projection** includes (`{"name":1}`) or excludes (`{"password":0}`) fields, dotted paths too.

**Tested with:** 61 hand-checked filters and 5 sort orders over a dataset built around the tricky cases, plus property checks over thousands of random documents and filters (`$ne` is the opposite of `$eq`, `$in` is an OR of equalities, `$or`/`$and`/`$nor` follow boolean logic, results are really sorted, skip/limit is a slice of the full result). Six planted semantics bugs are all caught; one of them exposed a missing test for sort stability, which was added.

**Not yet supported:** `$elemMatch`, `$not`, `$regex`, `$size`, `$type`, and updates with operators like `$set`.

Every query scans the whole collection for now. On 1 million documents that takes 0.6–0.7 s, whether 1 document matches or 11,000. That's what phase 5's indexes are for.

## Phase 3: documents and collections

`internal/doc` stores JSON-like documents in named collections, all inside the one B+tree, separated by key prefix:

```
c/<collection>                           collection metadata (document count)
d/<collection> \0 <_id> \0 <chunk>       document data
```

- **Binary encoding** with distinct types (null, bool, int64, float64, string, array, object), so `42` and `42.0` stay different, as in MongoDB. Each document has exactly one encoding: keys are sorted, and the decoder rejects anything non-canonical.
- **Large documents** are split into chunks of up to 3,000 bytes (the B+tree's value limit), up to 16 MB per document.
- **`_id`s** are MongoDB-style ObjectIDs (timestamp, random, counter), or your own string. Within one process, generated `_id`s sort in insertion order.
- Inserting a batch, replacing a document, and updating the collection count are each one atomic transaction.

```sh
./burrowdb doc shop.db insert users '{"name":"Ann","age":31,"tags":["admin"]}'
./burrowdb doc shop.db find users
./burrowdb doc shop.db get users <_id>
./burrowdb doc shop.db collections
```

**Tested with:** fuzzing the decoder (millions of inputs: corrupt bytes must never crash it, and anything accepted must re-encode identically; this caught non-minimal lengths being accepted), 3,000 random operations against an in-memory model with reopening, documents that grow and shrink across many chunks with no leaked pages, and atomic batch inserts.

**1 million documents** (`BURROWDB_MILLION=1 go test -run TestMillionDocuments ./internal/doc`, Apple M1 Pro): inserted in 10.8 s (93k/s, batches of 5,000), all read back in 0.66 s (1.5M/s), full page check 0.1 s, file 232 MB.

## Phase 2: a copy-on-write B+tree on disk

`internal/btree` is a B+tree of 4 KB pages with get, set, delete (merging under-full pages) and ordered range scans. It never changes a page in place: an update writes new copies of the pages from the root down to the changed leaf.

`internal/pager` stores the tree in one file. Two alternating meta pages hold the root, a transaction id and a checksum. A commit writes the new pages to places no committed tree uses, syncs, then writes the meta page and syncs again. Until the meta page lands, the old one still describes an untouched tree, so a crash at any point loses at most the commit in progress. Freed pages go on a free-list and are reused, so the file stops growing once it reaches its working size.

```sh
./burrowdb db demo.db set apple red
./burrowdb db demo.db set banana yellow
./burrowdb db demo.db scan            # all keys, in order
./burrowdb db demo.db scan a b        # keys in [a, b)
./burrowdb db demo.db check           # verify structure and page accounting
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
go build ./cmd/burrowdb
./burrowdb kv demo.log set greeting hello
./burrowdb kv demo.log get greeting
./burrowdb kv demo.log stats
```

### How it's tested

- **Crash test:** the test re-runs itself as a writer process, kills it with `SIGKILL` at random moments, reopens the store and checks every acknowledged write. It passes 1,000 rounds in a row (`BURROWDB_CRASH_ITERS=1000 go test -run TestCrashRecovery ./internal/kv`).
- **Torn writes:** the file is cut at every byte inside the last record, and each time recovery must keep all earlier records and accept new writes.
- **Corruption:** a flipped bit or trailing garbage is detected by the checksum and cut off.
- **Concurrency:** readers, writers and compaction run in parallel under `go test -race`.

```sh
go test -race ./...
```

## License

MIT
