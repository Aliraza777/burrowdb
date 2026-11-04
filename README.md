# burrowdb

A MongoDB-style document database, built from scratch in Go to learn how databases work underneath: a copy-on-write B+tree on disk, crash recovery, a query engine, secondary indexes with a query planner, transactions with group commit, an HTTP server, and a typed TypeScript client.

It's a learning and portfolio project, not a production database, but it's tested like one: crash loops that `SIGKILL` the writer hundreds of times, fuzzing against reference models, planted-bug checks for every layer, and an honest [benchmark against SQLite](docs/BENCHMARKS.md).

```sh
go install github.com/Aliraza777/burrowdb/cmd/burrowdb@latest

burrowdb doc shop.db insert users '{"name":"Ann","age":31,"tags":["admin"]}'
burrowdb doc shop.db index users age
burrowdb doc shop.db query users '{"filter":{"age":{"$gte":30}},"sort":{"age":-1}}'
burrowdb doc shop.db explain users '{"filter":{"age":{"$gte":30}}}'

burrowdb serve --data shop.db          # then use the TypeScript client
```

```ts
import { BurrowDB } from "burrowdb-client";

const users = new BurrowDB({ url: "http://127.0.0.1:7070" }).collection<User>("users");
await users.find({ age: { $gte: 18 }, tags: "admin" }, { sort: { age: -1 }, limit: 10 });
```

## How it fits together

```
  TypeScript client ──HTTP──▶ server        internal/server   JSON API, auth, POST /transaction
                                │
                          query engine      internal/query    filters, sort, projection, planner, explain
                                │
                  documents + indexes       internal/doc      collections, _id, chunking, index upkeep, Tx
                                │
                         value rules        internal/value    MongoDB type order, order-preserving index keys
                                │
                     copy-on-write B+tree   internal/btree    4 KB pages, never modified in place
                                │
                     pager + group commit   internal/pager    meta pages, free list, undo journal, one committer
                                │
                              one file
```

Everything lives in **one B+tree in one file**, separated by key prefix: `c/` for collection metadata, `d/` for documents, `i/` and `x/` for indexes. A commit writes new pages, flushes them, then flips one of two checksummed meta pages, so a crash at any moment leaves the last committed state intact.

## Features

- **Documents:** JSON-like values with int64 and float64 kept apart, MongoDB-style ObjectIDs, documents up to 16 MB.
- **Queries:** `$eq $ne $gt $gte $lt $lte $in $nin $exists $and $or $nor`, dotted paths through objects and arrays, sort, skip, limit and projection, all with MongoDB's matching rules.
- **Indexes:** single-field and multikey, maintained in the same transaction as each write, with a cost-based planner, `explain` and `hint`.
- **Transactions:** multi-document and multi-collection, read-your-writes, all-or-nothing, with group commit for concurrent writers.
- **Server and client:** a JSON HTTP API with stable error codes and bearer auth, and the [`burrowdb-client`](clients/ts) npm package with typed filters.

**Not supported:** `$elemMatch`, `$regex`, `$not`, update operators like `$set`, compound or unique indexes, replication, and sharding.

## Performance

Compared with SQLite (used as a document store) on the same machine, with the same durability ([full results and analysis](docs/BENCHMARKS.md)):

- **About 9× faster with 32 concurrent writers,** thanks to group commit.
- **About 2× slower for a single writer's commits:** two flushes per commit against SQLite's one.
- **1.3–2× slower on reads,** with no page cache yet.
- **About 2.9× larger on disk.**

The benchmark document explains each result and what would close the gaps.

## Status

| Phase | | |
| --- | --- | --- |
| 0 | Project setup, CI | ✅ |
| 1 | Key-value store on an append-only log | ✅ |
| 2 | B+tree storage engine | ✅ |
| 3 | Documents and collections | ✅ |
| 4 | Query engine | ✅ |
| 5 | Indexes and a query planner | ✅ |
| 6 | Transactions and group commit | ✅ |
| 7 | HTTP server and TypeScript client | ✅ |
| 8 | Benchmarks and launch | ✅ |

## How it was built, layer by layer

Each phase added one layer on top of a tested one. The sections below are in build order.

### Phase 1: an append-only key-value store

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

#### How it's tested

- **Crash test:** the test re-runs itself as a writer process, kills it with `SIGKILL` at random moments, reopens the store and checks every acknowledged write. It passes 1,000 rounds in a row (`BURROWDB_CRASH_ITERS=1000 go test -run TestCrashRecovery ./internal/kv`).
- **Torn writes:** the file is cut at every byte inside the last record, and each time recovery must keep all earlier records and accept new writes.
- **Corruption:** a flipped bit or trailing garbage is detected by the checksum and cut off.
- **Concurrency:** readers, writers and compaction run in parallel under `go test -race`.

```sh
go test -race ./...
```

### Phase 2: a copy-on-write B+tree on disk

`internal/btree` is a B+tree of 4 KB pages with get, set, delete (merging under-full pages) and ordered range scans. It never changes a page in place: an update writes new copies of the pages from the root down to the changed leaf.

`internal/pager` stores the tree in one file. Two alternating meta pages hold the root, a transaction id and a checksum. A commit writes the new pages to places no committed tree uses, syncs, then writes the meta page and syncs again. Until the meta page lands, the old one still describes an untouched tree, so a crash at any point loses at most the commit in progress. Freed pages go on a free-list and are reused, so the file stops growing once it reaches its working size.

```sh
./burrowdb db demo.db set apple red
./burrowdb db demo.db set banana yellow
./burrowdb db demo.db scan            # all keys, in order
./burrowdb db demo.db scan a b        # keys in [a, b)
./burrowdb db demo.db check           # verify structure and page accounting
```

#### How it's tested

- **Fuzzing against a model:** random sequences of sets and deletes run against the tree and a plain Go map. After each, the contents and order must match, the structure must be valid (sorted keys, balanced depth, nodes within a page), and every page must be accounted for. Hundreds of thousands of executions per minute.
- **Crash test:** a process committing 10-key batches is killed with `SIGKILL` 500 times. After each kill the file opens, passes the full check, and holds whole batches only.
- **Commit order:** a recording file checks that each commit does `write pages → sync → write meta → sync`, which a crash test can't see because the OS keeps a killed process's writes.
- **Torn meta page, interrupted commit, trailing junk, corrupt file:** each recovers to the last good commit or is refused.
- **Mutation checks:** planting known bugs (reusing pages too early, a single meta slot, a missing sync, broken merging, an off-by-one in search) makes the tests fail.

#### Benchmarks (Apple M1 Pro)

| | |
| --- | --- |
| B+tree in memory: set / get | 6.1 µs / 0.64 µs |
| On disk: one set per commit | 9.3 ms (each commit waits for a full flush to storage) |
| On disk: 1,000 sets per commit | 22 ms, 22 µs per key |
| On disk: get | 3.8 µs (no page cache yet) |

Per-commit cost is dominated by `fsync`, which is why batching writes into transactions (phase 6) matters.

### Phase 3: documents and collections

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

### Phase 4: a MongoDB-style query engine

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

**Not yet supported in queries:** `$elemMatch`, `$not`, `$regex`, `$size`, `$type`, and updates with operators like `$set`.

### Phase 5: indexes and a query planner

```sh
./burrowdb doc shop.db import users users.jsonl        # one JSON document per line
./burrowdb doc shop.db index users age
./burrowdb doc shop.db explain users '{"filter":{"age":{"$gte":30,"$lt":40}}}'
./burrowdb doc shop.db check                           # pages and every index verified
```

An index is a range of keys in the same B+tree: `x/<collection>\0<field>\0<value><_id>`. Values use an **order-preserving byte encoding** (`internal/value`), so byte order equals MongoDB's value order and `age >= 30 and age < 40` is one range scan. Following MongoDB, arrays are indexed once per element (**multikey**) and a missing field is indexed as null. Indexes are updated in the same transaction as every insert, replace and delete.

The **planner** turns each indexed field's conditions into key ranges (`$eq`, `$in`, `$gt/$gte/$lt/$lte`), counts the entries each candidate would read (stopping early), and picks the cheapest. An index that would read half the collection or more loses to a scan. On a multikey field it doesn't intersect two range conditions, because each can be met by a different array element. Every fetched document is re-checked against the full filter, so an index only has to narrow the search, never be exact. `explain` shows the plan, the bounds, the candidates and the work done; `hint` forces a plan (`"$natural"` for a scan, or a field name).

**Tested with:**
- all 61 hand-checked filters run through every plan: the planner's choice, a forced scan, and each usable index forced in turn;
- 1,500 random filters (single conditions and pairs) where every usable index must agree with a scan;
- `CheckIndexes`, which recomputes every index from the documents, after random inserts, replaces and deletes;
- fuzzing that the key encoding's byte order matches comparison order (23M+ pairs).

Six planted bugs are all caught: intersecting multikey ranges, an off-by-one bound, trusting the index without re-checking, not indexing missing fields as null, and stale entries on replace or delete.

**1 million documents** (Apple M1 Pro; `BURROWDB_MILLION=1 go test -run TestMillionScan -v ./internal/query`):

| Query | Scan | Index | |
| --- | --- | --- | --- |
| `n = 123456` (1 result) | 659 ms | 0.06 ms | ~12,000× |
| `n` in [500000, 500100) (100 results) | 556 ms | 0.83 ms | ~670× |
| `age` 30–31 and city Lahore (11,111 results) | 697 ms | 160 ms | 4× |
| `age = 42`, sort by `n`, limit 10 | 724 ms | 188 ms | 4× |

**Known limits:** the planner doesn't consider `limit`, so a common value with a small limit can be slower through an index than a scan that stops early. MongoDB races candidate plans for this. Sorting doesn't use indexes yet, `$or` doesn't combine indexes, and there are no compound or unique indexes.

### Phase 6: transactions and group commit

```go
err := db.Transaction(func(tx *doc.Tx) error {
    from, _ := tx.Get("bank", "alice")
    to, _ := tx.Get("bank", "bob")
    from["balance"] = from["balance"].(int64) - 50
    to["balance"] = to["balance"].(int64) + 50
    if err := tx.Replace("bank", "alice", from); err != nil {
        return err
    }
    return tx.Replace("bank", "bob", to)
})
```

A transaction can insert, replace, delete, read and query (`query.FindIn(tx.Reader(), ...)`) across collections, and sees its own uncommitted writes, indexes included. It commits all of it or none: if the function returns an error or panics, or **any write inside it failed** (even if the error was ignored), nothing is kept.

**Why not a write-ahead log?** The plan called for one, but the storage is copy-on-write: a commit writes new pages and then switches one checksummed meta page, so commits are already atomic and crash-safe without a log. LMDB and BoltDB use the same design and have no WAL. The real cost was elsewhere: every commit waits for two disk flushes (~9 ms), so concurrent writers queued behind each other.

**Group commit** fixes that. One committer goroutine takes every transaction waiting while the previous commit was flushing, runs them one after another against the same in-memory tree, and commits them all with one pair of flushes. `Update` still returns only once the data is on disk. If one transaction in a batch fails, only it is undone: every page allocation and free is journaled, and a failed transaction's journal entries are reversed exactly, newest first.

| Writers | Throughput | Disk commits |
| --- | --- | --- |
| 1 (one commit per transaction) | ~113 transactions/s | 1 per transaction |
| 32 concurrent | **~1,730 transactions/s** (~15×) | 49 for 800 transactions |

**Tested with:**
- batches mixing successful, failing and panicking transactions (only the successful ones' data survives, and the page check finds nothing leaked by the rollbacks);
- 300 random batches with random failures against a model, including failed transactions that free pages allocated earlier in the same batch;
- concurrent bank transfers (16 writers moving money between accounts while 4 readers sum all balances), where every reader must always see exactly the starting total.

Planted bugs in each of the four undo cases, a kept root, an ignored abort, and a `Tx` usable after its transaction are all caught. One of them (keeping a freed page freed) put a cycle into the tree and crashed the delete code. Detecting corrupted trees cleanly instead is a hardening item for later.

### Phase 7: an HTTP server and a TypeScript client

```sh
burrowdb serve --data app.db --addr 127.0.0.1:7070 --token "$BURROWDB_TOKEN"
```

```ts
import { BurrowDB } from "burrowdb-client";

const db = new BurrowDB({ url: "http://127.0.0.1:7070", token: process.env.BURROWDB_TOKEN });
const users = db.collection<User>("users");
await users.insertOne({ name: "Ann", age: 31 });
await users.find({ age: { $gte: 18 } }, { sort: { age: -1 }, limit: 10 });
```

**The server** (`internal/server`, standard library only) serves a JSON API for documents, find/count/explain, indexes, collections, and `POST /transaction` (a list of inserts, replaces and deletes applied all or nothing). Errors carry a stable code and a matching status: 404, 409, 400, 413 or 401. It listens on `127.0.0.1` by default, can require a bearer token (compared in constant time), limits request size and time, and on Ctrl-C finishes requests in flight before closing the database.

**The client** ([`clients/ts`](clients/ts), npm package `burrowdb-client`) has typed filters (`{ age: { $gt: "x" } }` is a compile error when `age` is a number), typed errors with the server's code, and transactions, with no dependencies. Its tests build the real Go server, start it, and run over HTTP; compile-time tests check that wrong filter types are rejected.

## Development

```sh
go test -race ./...                                                    # everything
BURROWDB_CRASH_ITERS=1000 go test -run TestCrashRecovery ./internal/pager   # crash soak
go test -run '^$' -fuzz FuzzTree ./internal/btree                     # fuzz the B+tree
BURROWDB_MILLION=1 go test -run TestMillionScan -v ./internal/query    # 1M-document benchmark
cd bench && go run .                                                   # vs SQLite
cd clients/ts && npm test                                              # client against a real server
```

The roadmap and design decisions are in [PLAN.md](PLAN.md).

## License

MIT
