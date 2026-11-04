# burrowdb-client

TypeScript client for [burrowdb](https://github.com/Aliraza777/burrowdb), a MongoDB-style document database built from scratch in Go. It talks to `burrowdb serve` over HTTP. It has no dependencies and uses the built-in `fetch` (Node 20+, browsers, Bun, Deno).

```sh
npm install burrowdb-client
```

Start a server (see the [main README](https://github.com/Aliraza777/burrowdb#readme) to build it):

```sh
burrowdb serve --data app.db --token "$BURROWDB_TOKEN"
```

## Usage

```ts
import { BurrowDB, BurrowDBError } from "burrowdb-client";

interface User {
  _id?: string;
  name: string;
  age: number;
  tags?: string[];
  address?: { city: string };
}

const db = new BurrowDB({ url: "http://127.0.0.1:7070", token: process.env.BURROWDB_TOKEN });
const users = db.collection<User>("users");

const id = await users.insertOne({ name: "Ann", age: 31, tags: ["admin"] });
await users.insertMany([{ name: "Bob", age: 27 }, { name: "Cid", age: 45 }]); // all or none

const ann = await users.findById(id); // null if missing

const adults = await users.find(
  { age: { $gte: 18 }, "address.city": "Lahore" },
  { sort: { age: -1 }, limit: 10, projection: { name: 1 } },
);
const admins = await users.count({ tags: "admin" }); // matches an element of the array
const first = await users.findOne({ $or: [{ age: { $lt: 20 } }, { name: "Ann" }] });

await users.replaceOne(id, { name: "Ann", age: 32 });
await users.deleteOne(id); // true if it existed
```

Filters are typed against your document: `{ age: { $gt: "x" } }` is a compile error when `age` is a number. Dotted paths like `"address.city"` are accepted untyped.

Supported operators: `$eq $ne $gt $gte $lt $lte $in $nin $exists`, combined with `$and $or $nor`.

### Indexes and explain

```ts
await users.createIndex("age");
await users.explain({ age: { $gte: 30, $lt: 40 } });
// { plan: "IXSCAN", index: "age", keysExamined: 12, docsExamined: 12, returned: 12, ... }
await users.indexes(); // [{ field: "age", multikey: false }]
```

### Transactions

Writes in one transaction are applied all together or not at all:

```ts
await db.transaction((tx) => {
  tx.replace("accounts", "alice", { balance: 50 });
  tx.replace("accounts", "bob", { balance: 150 });
  tx.insert("ledger", { from: "alice", to: "bob", amount: 50 });
});
```

### Errors

Failures throw a `BurrowDBError` with a stable `code` and the HTTP `status`:

| code | status | when |
| --- | --- | --- |
| `not_found` | 404 | no such document (`replaceOne`, a transaction op) |
| `duplicate_id` | 409 | inserting an `_id` that exists |
| `index_exists` / `index_not_found` | 409 / 404 | index management |
| `bad_request` | 400 | invalid filter, collection name, `_id`, … |
| `too_large` | 413 | document over 16 MB |
| `unauthorized` | 401 | missing or wrong token |
| `unreachable` | 0 | the server couldn't be reached |

```ts
try {
  await users.insertOne({ _id: "ann", name: "Ann", age: 31 });
} catch (e) {
  if (e instanceof BurrowDBError && e.code === "duplicate_id") {
    // already there
  }
}
```

### Numbers

JSON has one number type. burrowdb keeps whole numbers as 64-bit integers, but JavaScript numbers lose precision beyond 2^53, so very large integers don't round-trip exactly.

## License

MIT
