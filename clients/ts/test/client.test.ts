import { type ChildProcess, execFileSync, spawn } from "node:child_process";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { afterAll, beforeAll, describe, expect, it } from "vitest";
import { BurrowDB, BurrowDBError } from "../src/index.js";

// These tests run against a real server built from the Go source.
const repoRoot = fileURLToPath(new URL("../../..", import.meta.url));
const TOKEN = "test-token";
let server: ChildProcess;
let url: string;

beforeAll(async () => {
  const dir = mkdtempSync(join(tmpdir(), "burrowdb-client-"));
  const bin = join(dir, "burrowdb");
  execFileSync("go", ["build", "-o", bin, "./cmd/burrowdb"], { cwd: repoRoot, stdio: "inherit" });
  server = spawn(bin, [
    "serve",
    "--data",
    join(dir, "test.db"),
    "--addr",
    "127.0.0.1:0",
    "--token",
    TOKEN,
  ]);
  url = await new Promise<string>((resolve, reject) => {
    let log = "";
    server.stderr?.on("data", (chunk: Buffer) => {
      log += chunk.toString();
      const m = log.match(/on (http:\/\/127\.0\.0\.1:\d+)/);
      if (m?.[1]) resolve(m[1]);
    });
    server.on("exit", (code) => reject(new Error(`server exited (${code}): ${log}`)));
  });
});

afterAll(async () => {
  if (!server || server.exitCode !== null) return;
  const exited = new Promise((r) => server.on("exit", r));
  server.kill("SIGINT");
  await exited;
});

interface User {
  _id?: string;
  name: string;
  age: number;
  tags?: string[];
  address?: { city: string };
}

const connect = () => new BurrowDB({ url, token: TOKEN });

describe("burrowdb-client", () => {
  it("reports health and requires the token", async () => {
    expect((await connect().health()).ok).toBe(true);
    const anon = new BurrowDB({ url });
    await expect(anon.collections()).rejects.toMatchObject({ code: "unauthorized", status: 401 });
  });

  it("inserts, reads, replaces and deletes", async () => {
    const users = connect().collection<User>("crud");
    const id = await users.insertOne({ name: "Ann", age: 31, tags: ["admin"] });
    expect(id).toMatch(/^[0-9a-f]{24}$/);
    expect(await users.findById(id)).toEqual({ _id: id, name: "Ann", age: 31, tags: ["admin"] });
    await users.replaceOne(id, { name: "Ann", age: 32 });
    expect((await users.findById(id))?.age).toBe(32);
    expect(await users.deleteOne(id)).toBe(true);
    expect(await users.deleteOne(id)).toBe(false);
    expect(await users.findById(id)).toBeNull();
    await expect(users.replaceOne(id, { name: "x", age: 1 })).rejects.toMatchObject({
      code: "not_found",
      status: 404,
    });
  });

  it("handles ids that need URL escaping", async () => {
    const c = connect().collection("escaping");
    const id = "a/b?c#d%e";
    expect(await c.insertOne({ _id: id, v: 1 })).toBe(id);
    expect((await c.findById(id))?.v).toBe(1);
    expect(await c.deleteOne(id)).toBe(true);
  });

  it("queries with MongoDB-style filters, sort, limit and projection", async () => {
    const users = connect().collection<User>("query");
    await users.insertMany([
      { name: "Ann", age: 31, tags: ["admin", "dev"], address: { city: "Lahore" } },
      { name: "Bob", age: 27, tags: ["dev"], address: { city: "Karachi" } },
      { name: "Cid", age: 45, address: { city: "Lahore" } },
      { name: "Dee", age: 19 },
    ]);
    const older = await users.find(
      { age: { $gt: 30 } },
      { sort: { age: -1 }, projection: { name: 1 } },
    );
    expect(older.map((u) => u.name)).toEqual(["Cid", "Ann"]);
    expect(older[0]).not.toHaveProperty("age");
    expect((await users.find({ tags: "dev" }, { sort: { name: 1 } })).map((u) => u.name)).toEqual([
      "Ann",
      "Bob",
    ]);
    expect(await users.count({ "address.city": "Lahore" })).toBe(2);
    expect(await users.count({ $or: [{ age: { $lt: 20 } }, { tags: { $in: ["admin"] } }] })).toBe(
      2,
    );
    expect((await users.findOne({}, { sort: { age: 1 } }))?.name).toBe("Dee");
    expect(await users.findOne({ name: "Nobody" })).toBeNull();
    expect(
      (await users.find({}, { sort: { age: 1 }, skip: 1, limit: 2 })).map((u) => u.name),
    ).toEqual(["Bob", "Ann"]);
  });

  it("creates indexes the planner uses", async () => {
    const c = connect().collection<{ n: number }>("indexed");
    await c.insertMany(Array.from({ length: 200 }, (_, n) => ({ n })));
    expect((await c.explain({ n: 150 })).plan).toBe("COLLSCAN");
    await c.createIndex("n");
    const ex = await c.explain({ n: { $gte: 150, $lt: 155 } });
    expect(ex).toMatchObject({ plan: "IXSCAN", index: "n", docsExamined: 5, returned: 5 });
    expect(await c.indexes()).toEqual([{ field: "n", multikey: false }]);
    await expect(c.createIndex("n")).rejects.toMatchObject({ code: "index_exists", status: 409 });
    await c.dropIndex("n");
    expect(await c.indexes()).toEqual([]);
  });

  it("applies transactions all or nothing", async () => {
    const db = connect();
    const bank = db.collection<{ balance: number }>("bank");
    await bank.insertMany([
      { _id: "alice", balance: 100 },
      { _id: "bob", balance: 0 },
    ]);
    const results = await db.transaction((tx) => {
      tx.replace("bank", "alice", { balance: 60 });
      tx.replace("bank", "bob", { balance: 40 });
      tx.insert("ledger", { _id: "t1", amount: 40 });
    });
    expect(results).toEqual([{ replaced: true }, { replaced: true }, { id: "t1" }]);

    const failed = db.transaction((tx) => {
      tx.replace("bank", "alice", { balance: 0 });
      tx.replace("bank", "nobody", { balance: 100 });
    });
    await expect(failed).rejects.toMatchObject({ code: "not_found" });
    expect((await bank.findById("alice"))?.balance).toBe(60);
  });

  it("raises typed errors", async () => {
    const db = connect();
    const c = db.collection("errors");
    await c.insertOne({ _id: "x" });
    const dup = await c.insertOne({ _id: "x" }).catch((e: unknown) => e);
    expect(dup).toBeInstanceOf(BurrowDBError);
    expect(dup).toMatchObject({ code: "duplicate_id", status: 409 });
    await expect(c.find({ x: { $bogus: 1 } } as never)).rejects.toMatchObject({
      code: "bad_request",
      status: 400,
    });
    await expect(db.collection("bad name").insertOne({})).rejects.toMatchObject({
      code: "bad_request",
    });
    const offline = new BurrowDB({ url: "http://127.0.0.1:1", timeoutMs: 2000 });
    await expect(offline.health()).rejects.toMatchObject({ code: "unreachable", status: 0 });
  });

  it("lists and drops collections", async () => {
    const db = connect();
    await db.collection("temp").insertOne({ a: 1 });
    expect(await db.collections()).toContain("temp");
    expect(await db.dropCollection("temp")).toBe(true);
    expect(await db.collections()).not.toContain("temp");
  });
});
