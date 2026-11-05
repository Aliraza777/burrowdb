# burrowdb

A MongoDB-style document database written from scratch in Go.

I built burrowdb to understand what really happens between `insertOne()` and bytes on disk. It has a copy-on-write B+tree storage engine with crash recovery, a MongoDB-compatible query language, secondary indexes with a cost-based query planner, multi-document transactions with group commit, an HTTP server, and a typed TypeScript client.

It's a learning project rather than a production database, but it's tested seriously: crash loops that `SIGKILL` the writer hundreds of times, fuzzing against reference models, mutation testing for every layer, and an honest [benchmark against SQLite](docs/BENCHMARKS.md).

## Quick start

```sh
go install github.com/Aliraza777/burrowdb/cmd/burrowdb@latest

burrowdb doc shop.db insert users '{"name":"Ann","age":31,"tags":["admin"]}'
burrowdb doc shop.db insert users '{"name":"Bob","age":45}'
burrowdb doc shop.db index users age
burrowdb doc shop.db query users '{"filter":{"age":{"$gte":30}},"sort":{"age":-1}}'
burrowdb doc shop.db explain users '{"filter":{"age":{"$gte":30}}}'
```

Run it as a server and use it from Node or the browser:

```sh
burrowdb serve --data shop.db --token "$BURROWDB_TOKEN"
```

```ts
import { BurrowDB } from "burrowdb-client";

const db = new BurrowDB({ url: "http://127.0.0.1:7070", token: process.env.BURROWDB_TOKEN });
const users = db.collection<User>("users");

await users.insertOne({ name: "Ann", age: 31, tags: ["admin"] });
await users.find({ age: { $gte: 18 }, tags: "admin" }, { sort: { age: -1 }, limit: 10 });
```

## Features

- **Documents:** JSON-like values with int64 and float64 kept distinct, MongoDB-style ObjectIDs, documents up to 16 MB.
- **Queries:** `$eq $ne $gt $gte $lt $lte $in $nin $exists $and $or $nor`, dotted paths through objects and arrays, sort, skip, limit and projection, all following MongoDB's matching rules.
- **Indexes:** single-field and multikey, updated in the same transaction as each write, with a cost-based planner, `explain` and `hint`.
- **Transactions:** multi-document and multi-collection, read-your-own-writes, all or nothing.
- **Group commit:** concurrent writers share disk flushes, about 9× faster than SQLite with 32 writers.
- **Crash safety:** a commit is all there or not there at all after a crash or power loss.
- **Server and client:** a JSON HTTP API with stable error codes and bearer-token auth, and the [`burrowdb-client`](clients/ts) npm package with typed filters.

## How it works

```
  TypeScript client ──HTTP──▶ server        internal/server   JSON API, auth, transactions
                                │
                          query engine      internal/query    filters, sort, projection, planner, explain
                                │
                  documents + indexes       internal/doc      collections, _id, chunking, index upkeep, Tx
                                │
                         value rules        internal/value    MongoDB type order, order-preserving index keys
                                │
                     copy-on-write B+tree   internal/btree    4 KB pages, never modified in place
                                │
                     pager + group commit   internal/pager    meta pages, free list, undo journal, committer
                                │
                              one file
```

### Storage

Everything lives in **one B+tree in one file**. The tree (`internal/btree`) uses 4 KB pages and supports get, set, delete (merging under-full pages) and ordered range scans. It never changes a page in place: an update writes new copies of the pages from the root down to the changed leaf.

The pager (`internal/pager`) gives that tree crash safety. Two alternating meta pages hold the current root, a transaction id and a checksum. A commit writes new pages to places no committed tree uses, flushes them, then writes the meta page and flushes again. Until the meta page lands, the previous one still describes an untouched tree. So after a crash or power loss, the database opens at the last complete commit. A torn meta page fails its checksum, and the other one is used. Freed pages go on a free list and are reused, so the file stops growing once it reaches its working size.

```sh
burrowdb db demo.db check     # verify the tree and that every page is accounted for
```

### Documents

`internal/doc` keeps collections in the same tree, separated by key prefix:

```
c/<collection>                           collection metadata
d/<collection> \0 <_id> \0 <chunk>       document data
i/<collection> \0 <field>                index metadata
x/<collection> \0 <field> \0 <value><_id> index entries
```

Documents use a compact binary encoding with distinct types (null, bool, int64, float64, string, array, object), so `42` and `42.0` stay different, as in MongoDB. Every document has exactly one encoding: keys are sorted, and the decoder rejects anything non-canonical. Large documents are split into chunks of up to 3,000 bytes. `_id`s are MongoDB-style ObjectIDs (timestamp, random, counter), or any string you provide.

### Queries

`internal/query` implements MongoDB's matching rules, including the subtle ones:

- `{"x": null}` matches documents where `x` is null **and** documents with no `x`.
- An array matches if any element matches (`{"tags": "admin"}`), or as a whole (`{"tags": ["admin", "dev"]}`).
- Dotted paths reach into nested objects and through arrays of objects (`items.price`, `items.0.price`).
- `1` equals `1.0`, and range operators only compare within one type: `{"age": {"$gt": 30}}` never matches `"unknown"`.
- Sorting uses MongoDB's type order (null < numbers < strings < objects < arrays < booleans). Arrays sort by their smallest element ascending and largest descending, and ties keep `_id` order.

```sh
burrowdb doc shop.db query users '{"filter":{"age":{"$gt":30},"tags":"admin"},"sort":{"age":-1},"limit":10,"projection":{"name":1}}'
burrowdb doc shop.db count users '{"address.city":"Lahore"}'
```

### Indexes and the query planner

An index is a range of keys in the same B+tree. Values are stored in an **order-preserving byte encoding** (`internal/value`): a type tag first, numbers as float64 bits arranged to sort numerically, strings with escaped zero bytes. That way byte order equals MongoDB's value order, and `age >= 30 and age < 40` becomes a single range scan. As in MongoDB, an array is indexed once per element (multikey) and a missing field is indexed as null.

The planner turns each indexed field's conditions into key ranges, counts the entries each candidate would read (stopping early), and picks the cheapest. If an index would read half the collection or more, a full scan wins. Every fetched document is re-checked against the full filter, so an index narrows the search but never decides the answer. One consequence of MongoDB's array semantics: on a multikey field, `{"price": {"$gt": 10, "$lt": 20}}` matches `[25, 10]`, because each condition can be met by a different element. So the planner doesn't intersect those two ranges.

```sh
burrowdb doc shop.db import users users.jsonl          # one JSON document per line
burrowdb doc shop.db index users age
burrowdb doc shop.db explain users '{"filter":{"age":{"$gte":30,"$lt":40}}}'
```

On 1 million documents, finding one document by an indexed field takes **0.06 ms instead of 659 ms**.

### Transactions and group commit

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

A transaction can insert, replace, delete, read and query across collections, and sees its own uncommitted writes, indexes included. It commits everything or nothing. If the function returns an error or panics, or any write inside it failed (even if the error was ignored), nothing is kept.

There's no write-ahead log. Because storage is copy-on-write, commits are already atomic without one, the same design as LMDB and BoltDB. Instead, a single committer goroutine collects every transaction that arrives while the previous commit is flushing, and commits them together with one pair of flushes (**group commit**). If one transaction in a batch fails, only that one is undone: every page allocation and free is journaled, and the failed transaction's entries are reversed exactly.

| Writers | Throughput |
| --- | --- |
| 1 (one commit per transaction) | ~113 transactions/s |
| 32 concurrent | ~1,730 transactions/s |

### Server and TypeScript client

`burrowdb serve` (`internal/server`, standard library only) provides a JSON API for documents, find, count, explain, indexes and collections. `POST /transaction` applies a list of inserts, replaces and deletes atomically. Errors carry a stable code and a matching status (404, 409, 400, 413, 401). By default the server listens only on `127.0.0.1`. It can require a bearer token, limits request size and time, and on Ctrl-C finishes in-flight requests before closing the database.

The [TypeScript client](clients/ts) (`npm install burrowdb-client`) has no dependencies. Its filters are typed against your documents, so `{ age: { $gt: "x" } }` is a compile error when `age` is a number.

## Testing

```sh
go test -race ./...
```

- **Crash recovery:** tests re-run themselves as a writer process, kill it with `SIGKILL` at random moments, and check after every kill that the file opens cleanly and holds only complete transactions. They pass 1,000 consecutive rounds.
- **Write ordering:** a recording file checks that every commit does `write pages → flush → write meta → flush`. A killed process can't reveal a missing flush, because the OS still holds its writes.
- **Fuzzing:** the B+tree against a Go map (with page-leak checks), the document decoder against corrupt input, and the index key encoding against comparison order.
- **Reference models:** random operations are checked against simple in-memory models at every layer. Queries are checked against 61 hand-verified filters and through every possible plan (each index forced in turn, and a full scan).
- **Concurrency:** concurrent bank transfers, where every reader must always see the same total balance.
- **Mutation testing:** known bugs are planted in each layer (a missing flush, reusing pages too early, intersecting multikey ranges, a broken rollback, and others) to confirm the tests catch them.
- **End to end:** the TypeScript client's tests build and start the real server and run over HTTP.

```sh
BURROWDB_CRASH_ITERS=1000 go test -run TestCrashRecovery ./internal/pager   # crash soak
go test -run '^$' -fuzz FuzzTree ./internal/btree                          # fuzz the B+tree
BURROWDB_MILLION=1 go test -run TestMillionScan -v ./internal/query         # 1M documents, scan vs index
cd clients/ts && npm test                                                   # client against a real server
```

## Performance

Compared with SQLite used as a document store, on the same machine with the same durability settings ([full results and analysis](docs/BENCHMARKS.md)):

- **About 9× faster with 32 concurrent writers,** thanks to group commit.
- **About 2× slower for a single writer's commits:** two flushes per commit against SQLite's one.
- **1.3–2× slower on reads,** because there's no page cache yet.
- **About 2.9× larger on disk.**

```sh
cd bench && go run . -n 100000
```

## Limitations

- **Not supported in queries:** `$elemMatch`, `$regex`, `$not`, `$size`, `$type`, and update operators such as `$set`. Documents are replaced whole.
- **Indexes:** no compound or unique indexes, and sorting and `$or` don't use indexes yet.
- **Planner:** it doesn't take `limit` into account, so a small limit on a common value can be faster as a scan.
- **Single node:** no replication or sharding. One process at a time may open a database file.
- **Corrupted files:** a file damaged in a way that creates a cycle in the tree causes a crash rather than a clean error.

Next on the list: a page cache, in-place page updates within a transaction, and a single-flush commit path (see the [benchmark analysis](docs/BENCHMARKS.md)).

## Project layout

```
cmd/burrowdb/      CLI: doc, serve, db, kv
internal/btree/    copy-on-write B+tree
internal/pager/    file storage, meta pages, free list, group commit
internal/value/    value comparison and order-preserving index keys
internal/doc/      documents, collections, indexes, transactions
internal/query/    query engine and planner
internal/server/   HTTP API
internal/kv/       a simpler append-only log store (Bitcask-style), with its own crash recovery
clients/ts/        TypeScript client (npm: burrowdb-client)
bench/             benchmark against SQLite
docs/              benchmark results
```

## License

MIT
