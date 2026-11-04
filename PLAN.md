# tinydoc: a document database built from scratch in Go

A small MongoDB-style database: JSON documents in collections, a query language, indexes, crash-safe storage, and a server you can talk to over HTTP. Built for learning and as a portfolio project, one working layer at a time.

## Goal

By the end, this should work:

```sh
tinydoc serve --data ./data
curl -X POST localhost:7070/db/shop/users -d '{"name":"Ann","age":31}'
curl -X POST localhost:7070/db/shop/users/find -d '{"filter":{"age":{"$gt":30}},"sort":{"age":-1}}'
```

and survive `kill -9` mid-write without losing committed data. A TypeScript client on npm lets JS apps use it.

## How it fits together

```
  HTTP API / shell / TS client
            │
     Query engine      ← filters ($gt, $in, $and…), sort, limit, projection
            │
  Indexes + planner    ← secondary indexes on fields; pick index or full scan
            │
   Document layer      ← collections, _id, binary document encoding
            │
  Storage engine       ← B+tree of pages on disk (ordered keys)
            │
  Write-ahead log      ← durability + crash recovery
            │
         Disk
```

Each layer only talks to the one below it, so each phase builds on a working, tested base.

## Phases

Each phase ends with something you can demo and post about. Times assume part-time work.

### Phase 0: Setup (1–2 days)
- Install Go (`brew install go`), create the module, CI with `go test -race`, lint (`golangci-lint`).
- Read: *Build Your Own Database From Scratch in Go* (James Smith, build-your-own.org), the chapters on the KV store and B+tree.
- **Done when:** `go test ./...` runs green in CI.

### Phase 1: Key-value store on an append-only log (1–2 weeks)
- Write each `set`/`delete` as a record appended to a file: length, CRC32 checksum, key, value.
- Keep an in-memory map from key to file offset; rebuild it on startup by replaying the log.
- Reject torn or corrupt records at the end of the file (the checksum catches them).
- Compaction: rewrite the file without dead records.
- **Learn:** fsync, durability, file formats, why checksums matter.
- **Done when:** a crash test (kill the process mid-write, restart, check data) passes 1,000 times in a row.

### Phase 2: B+tree storage engine (3–4 weeks), the core of the project
- Fixed-size 4 KB pages, a pager that reads and writes pages, and a free-list for reused pages.
- B+tree insert, delete (with merging), point lookup, and range scan with an iterator.
- Copy-on-write updates plus a meta page, as in BoltDB, so a crash always leaves a valid tree.
- **Learn:** how real databases store ordered data on disk.
- **Done when:** fuzz tests (Go's built-in fuzzing) compare the tree to an in-memory sorted map for millions of random operations with no difference. Benchmark: 100k inserts and range scans.

### Phase 3: Documents and collections (1–2 weeks)
- Encode documents in a compact binary format (your own, or MessagePack) and decode them back.
- Generate an `_id` (time-sortable, like Mongo's ObjectId).
- Keys: `collection/_id`, so a collection is a range in the B+tree.
- API: `insert`, `get`, `update`, `delete`, `scan` per collection.
- **Done when:** you can store and read 1M documents and list collections.

### Phase 4: Query engine (2–3 weeks)
- Mongo-style filters: `$eq $ne $gt $gte $lt $lte $in $exists $and $or`, including nested fields (`address.city`).
- Sort, limit, skip, projection.
- Start with a full collection scan; make it correct before making it fast.
- **Done when:** a test suite of about 100 queries returns exactly what a reference implementation (plain Go loops over the same data) returns.

### Phase 5: Secondary indexes and a simple planner (2–3 weeks)
- `createIndex("age")` builds a second B+tree from field values to `_id`.
- Keep indexes in sync on insert, update and delete, in the same transaction.
- Planner: if the filter has an indexed field, use an index range scan; otherwise do a full scan. `explain` shows which one was chosen.
- **Learn:** why indexes speed up reads and slow down writes.
- **Done when:** benchmarks show the index turning a query from O(n) into O(log n), and `explain` proves which plan ran.

### Phase 6: Transactions and a write-ahead log (2 weeks)
- A WAL plus checkpoints, so multi-document writes are all-or-nothing.
- One writer and many readers (readers see a consistent snapshot via copy-on-write).
- **Done when:** a crash at any point during a multi-document update leaves either all of it or none of it.

### Phase 7: Server, shell and TypeScript client (2 weeks)
- HTTP/JSON server (`tinydoc serve`), an interactive shell (`tinydoc shell`), and graceful shutdown.
- A TypeScript client published to npm (`tinydoc-client`), which plays to your existing strengths.
- Docker image.
- **Done when:** a small demo app (e.g. a todo API in Node) runs on tinydoc.

### Phase 8: Polish and launch (1 week)
- README with an architecture diagram and a "how it works" section per layer.
- Honest benchmarks against SQLite and MongoDB on the same machine. It will be slower, and that's fine; explain why.
- A blog post series: one post per phase ("How I built a B+tree in Go", and so on).

**Total:** roughly 3–4 months part-time. Phases 1 to 4 alone (about 2 months) already make a solid, demoable project.

## Folder layout

```
tinydoc/
  cmd/tinydoc/        CLI: serve, shell
  internal/kv/        Phase 1 log-structured store
  internal/pager/     pages, free-list
  internal/btree/     B+tree
  internal/wal/       write-ahead log, recovery
  internal/doc/       encoding, _id, collections
  internal/query/     filter parsing and evaluation, sort, projection
  internal/index/     secondary indexes, planner
  server/             HTTP API
  clients/ts/         TypeScript client (npm)
  bench/              benchmarks
  docs/               architecture notes, one per layer
```

## Testing approach

The thing that makes a database project credible is how it's tested:
- **Unit tests** for every layer.
- **Fuzzing:** compare against a simple in-memory model, for the B+tree and the query engine.
- **Crash tests:** kill the process at random points during writes, restart, and check that every committed write is there and nothing is corrupt.
- **Race detector:** `go test -race` in CI.
- **Benchmarks** tracked over time, so you can see each optimisation pay off.

## Learning resources

- *Build Your Own Database From Scratch in Go*, James Smith (build-your-own.org). The closest match to this plan.
- *Database Internals*, Alex Petrov: B-trees, LSM trees, recovery.
- *Designing Data-Intensive Applications*, Martin Kleppmann: the big picture.
- CMU 15-445 *Database Systems* lectures (free on YouTube).
- Source code to read: `bbolt` (Go B+tree KV store) and `badger` (Go LSM store).

## Out of scope (for now)

Replication, sharding, users and permissions, a full MongoDB wire protocol, and SQL. These are good "next" ideas once the core works.
