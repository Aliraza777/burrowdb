# Benchmarks: burrowdb vs SQLite

burrowdb is a learning project, so the point of measuring isn't to win. It's to see where each design choice costs or pays off. SQLite is the fairest comparison: both are embedded databases in a single file, and both are durable on every commit.

## Setup

- **Same workload, same machine, same process.** 100,000 documents like `{"n": 42, "age": 42, "city": "Lahore", "name": "user 42", "tags": ["t0", "t9"]}`, with indexes on `n` and `age` created before inserting.
- **SQLite as a document store**, the way you'd actually use it for JSON: a `TEXT` column holding the document, with expression indexes on `json_extract(body, '$.n')` and `'$.age'`. Driver: [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) v1.60.1 (pure Go).
- **Equal durability.** SQLite runs with `journal_mode=WAL`, `synchronous=FULL` and `fullfsync=ON`. burrowdb's Go `Sync` uses `F_FULLFSYNC` on macOS, and without `fullfsync` SQLite would use a cheaper, weaker flush.
- **Equal work per result.** Every document a query returns is decoded into a Go map for both databases.
- **Not measured:** MongoDB. It wasn't available on the benchmark machine, so no MongoDB numbers are shown rather than guessed ones.

Apple M1 Pro (8 cores), macOS, Go 1.27.3. Two runs:

| Workload | burrowdb | SQLite (JSON column) | |
| --- | --- | --- | --- |
| Bulk insert, 100k docs, 1,000 per transaction | 30.4k / 29.7k docs/s | 71.9k / 73.1k docs/s | SQLite ~2.4× |
| Single insert, one transaction each | 9.7 / 8.0 ms | 4.1 / 4.2 ms | SQLite ~2× |
| **32 concurrent writers, one insert per transaction** | **1.5k / 1.6k inserts/s** | 160 / 183 inserts/s | **burrowdb ~9×** |
| Point lookup on an indexed field | 36 / 39 µs | 17 / 25 µs | SQLite ~1.5–2× |
| Range query on an index (~2.2k docs returned) | 15.9 / 17.9 ms | 9.1 / 9.6 ms | SQLite ~1.8× |
| Count with an unindexed filter (full scan) | 97 / 105 ms | 78 / 75 ms | SQLite ~1.3× |
| File size | 50.3 MB | 17.4 MB | SQLite ~2.9× smaller |

Reproduce with:

```sh
cd bench && go run . -n 100000
```

## Why each row comes out the way it does

**Concurrent writers: burrowdb wins about 9×.** This is group commit. One committer goroutine takes every transaction that arrived while the previous commit was flushing and commits them together, so 640 inserts need only a few dozen disk flushes. SQLite serializes writers, and each pays for its own flush.

**Single inserts: SQLite wins about 2×.** Each burrowdb commit flushes twice: first the new pages, then the meta page that points at them. Order matters, so the two can't merge. SQLite in WAL mode appends the change to its log and flushes once. This is the real argument for a write-ahead log, which burrowdb doesn't have. Copy-on-write already makes commits atomic, and group commit handles throughput under concurrency, but a lone writer still pays for two flushes.

**Bulk inserts: SQLite wins about 2.4×.** burrowdb's B+tree is copy-on-write even within a transaction. Every key it sets copies the pages from leaf to root, and each document writes several keys: its data chunk, one entry per index, and the collection count. SQLite modifies pages in place inside a transaction and writes each dirty page once.

**Reads: SQLite wins 1.3–2×.** burrowdb has no page cache yet, so every page it touches is a `pread` into a new buffer. An index range query also fetches each matching document with its own B+tree lookup. SQLite keeps hot pages in memory.

**File size: SQLite is about 2.9× smaller.** Every burrowdb key repeats the collection name and the 24-character `_id`, and every index entry repeats the collection and field name. B+tree pages split half full, and SQLite's record format is more compact.

## What would close the gaps

| Change | Expected effect |
| --- | --- |
| A page cache (LRU of decoded pages) | Faster reads and bulk inserts |
| Mutating pages in place within a transaction (copy only on the first write) | Bulk inserts |
| A single-flush commit (a log, or checksummed pages with a validated meta) | Single-writer commit latency |
| Key prefix compression, short internal ids | File size |
| Fetching index ranges in key order, without a lookup per document | Range queries |

None of these change the design's guarantees, and they're good next steps for anyone reading the code.
