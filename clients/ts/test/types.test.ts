import { expect, it } from "vitest";
import type { Collection, Filter } from "../src/index.js";

// Compile-time checks: `npm run typecheck` fails if a @ts-expect-error line
// stops being an error. The function is never called.
interface User {
  _id?: string;
  name: string;
  age: number;
  tags: string[];
}

function typeChecks(users: Collection<User>) {
  const ok: Filter<User>[] = [
    { age: 31 },
    { age: { $gt: 30, $lte: 40 } },
    { name: { $in: ["Ann", "Bob"] } },
    { tags: "admin" }, // one element of an array field
    { tags: ["admin", "dev"] }, // the whole array
    { tags: { $in: ["a", "b"] } },
    { "address.city": "Lahore" }, // dotted paths are allowed
    { $or: [{ age: 1 }, { name: "x" }] },
    { age: null },
    { age: { $exists: false } },
  ];
  // @ts-expect-error age is a number
  const badValue: Filter<User> = { age: "old" };
  // @ts-expect-error age is a number
  const badOperator: Filter<User> = { age: { $gt: "x" } };
  // @ts-expect-error $in takes an array
  const badIn: Filter<User> = { name: { $in: "Ann" } };
  // @ts-expect-error $exists takes a boolean
  const badExists: Filter<User> = { age: { $exists: 1 } };

  users.find(
    { age: { $gte: 18 } },
    { sort: { age: -1, name: 1 }, limit: 10, projection: { name: 1 } },
  );
  // @ts-expect-error sort directions are 1 or -1
  users.find({}, { sort: { age: 2 } });
  // @ts-expect-error insertOne needs the document's required fields
  users.insertOne({ name: "no age" });
  return [ok, badValue, badOperator, badIn, badExists];
}

it("type checks compile (see npm run typecheck)", () => {
  expect(typeof typeChecks).toBe("function");
});
