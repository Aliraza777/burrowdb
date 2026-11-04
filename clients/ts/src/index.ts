/**
 * TypeScript client for burrowdb's HTTP API (`burrowdb serve`).
 *
 * ```ts
 * const db = new BurrowDB({ url: "http://127.0.0.1:7070" });
 * const users = db.collection<User>("users");
 * await users.insertOne({ name: "Ann", age: 31 });
 * const adults = await users.find({ age: { $gte: 18 } }, { sort: { age: -1 }, limit: 10 });
 * ```
 */

/** Any document. `_id` is a string; burrowdb generates one if you leave it out. */
export interface Document {
  _id?: string;
  [field: string]: unknown;
}

export type WithId<T> = T & { _id: string };
export type WithoutId<T> = Omit<T, "_id"> & { _id?: string };

type ElementOf<V> = V extends readonly (infer E)[] ? E : V;

/** Operators on one field. Range operators compare within one type, as in MongoDB. */
export interface Operators<V> {
  $eq?: V | null;
  $ne?: V | null;
  $gt?: V;
  $gte?: V;
  $lt?: V;
  $lte?: V;
  $in?: (V | null)[];
  $nin?: (V | null)[];
  $exists?: boolean;
}

/**
 * A condition on a field: a value (equality), or operators. On an array field
 * a single element matches too, as in MongoDB (`{ tags: "admin" }`).
 */
export type Condition<V> = V | ElementOf<V> | null | Operators<V | ElementOf<V>>;

/**
 * A filter, typed against the document. Dotted paths such as
 * `"address.city"` are allowed and untyped.
 */
export type Filter<T> = { [K in keyof T]?: Condition<T[K]> } & {
  $and?: Filter<T>[];
  $or?: Filter<T>[];
  $nor?: Filter<T>[];
} & { [path: `${string}.${string}`]: unknown };

type FieldName<T> = (keyof T & string) | `${string}.${string}`;

export interface FindOptions<T> {
  /** Fields in order of priority: 1 ascending, -1 descending. */
  sort?: { [K in FieldName<T>]?: 1 | -1 };
  skip?: number;
  /** 0 or absent means no limit. */
  limit?: number;
  /** Include fields (`{ name: 1 }`) or exclude them (`{ password: 0 }`). */
  projection?: { [K in FieldName<T> | "_id"]?: 0 | 1 | boolean };
  /** Force a plan: `"$natural"` for a collection scan, or an indexed field. */
  hint?: string;
}

export interface IndexInfo {
  field: string;
  /** Some document has an array (several values) at this field. */
  multikey: boolean;
}

export interface Explain {
  plan: "IXSCAN" | "COLLSCAN";
  index?: string;
  bounds?: string;
  considered?: string[];
  keysExamined: number;
  docsExamined: number;
  returned: number;
}

export interface ClientOptions {
  /** Server URL, e.g. "http://127.0.0.1:7070". */
  url: string;
  /** Bearer token, if the server was started with --token. */
  token?: string;
  /** Per-request timeout in milliseconds (default 30 000). */
  timeoutMs?: number;
  /** A custom fetch, e.g. for tests. Defaults to the global fetch. */
  fetch?: typeof fetch;
}

/** An error from the server, with its stable code and HTTP status. */
export class BurrowDBError extends Error {
  constructor(
    message: string,
    /** e.g. "not_found", "duplicate_id", "bad_request", "unauthorized". */
    readonly code: string,
    /** HTTP status, or 0 if the server couldn't be reached. */
    readonly status: number,
  ) {
    super(message);
    this.name = "BurrowDBError";
  }
}

export type TransactionResult = { id: string } | { replaced: true } | { deleted: boolean };

/** Collects writes for {@link BurrowDB.transaction}. */
export class TransactionBuilder {
  /** @internal */
  readonly ops: Record<string, unknown>[] = [];

  insert(collection: string, document: Document): this {
    this.ops.push({ op: "insert", collection, document });
    return this;
  }

  replace(collection: string, id: string, document: Document): this {
    this.ops.push({ op: "replace", collection, id, document });
    return this;
  }

  delete(collection: string, id: string): this {
    this.ops.push({ op: "delete", collection, id });
    return this;
  }
}

const seg = encodeURIComponent;

export class BurrowDB {
  private readonly base: string;
  private readonly opts: ClientOptions;

  constructor(opts: ClientOptions) {
    this.opts = opts;
    this.base = opts.url.replace(/\/+$/, "");
  }

  /** A handle on a collection; it's created on the first insert. */
  collection<T extends object = Document>(name: string): Collection<T> {
    return new Collection<T>(this, name);
  }

  async health(): Promise<{ ok: boolean; version: string }> {
    return this.request("GET", "/health");
  }

  async collections(): Promise<string[]> {
    const res = await this.request<{ collections: string[] }>("GET", "/collections");
    return res.collections;
  }

  /** Deletes a collection, its documents and its indexes. */
  async dropCollection(name: string): Promise<boolean> {
    const res = await this.request<{ dropped: boolean }>("DELETE", `/collections/${seg(name)}`);
    return res.dropped;
  }

  /**
   * Applies writes atomically: all of them or none. Returns one result per
   * write, in order.
   *
   * ```ts
   * await db.transaction((tx) => {
   *   tx.replace("bank", "alice", { balance: 50 });
   *   tx.replace("bank", "bob", { balance: 150 });
   * });
   * ```
   */
  async transaction(build: (tx: TransactionBuilder) => void): Promise<TransactionResult[]> {
    const tx = new TransactionBuilder();
    build(tx);
    const res = await this.request<{ results: TransactionResult[] }>("POST", "/transaction", {
      ops: tx.ops,
    });
    return res.results;
  }

  /** @internal */
  async request<R>(method: string, path: string, body?: unknown): Promise<R> {
    const headers: Record<string, string> = { Accept: "application/json" };
    if (body !== undefined) headers["Content-Type"] = "application/json";
    if (this.opts.token) headers.Authorization = `Bearer ${this.opts.token}`;
    const doFetch = this.opts.fetch ?? fetch;
    let res: Response;
    try {
      res = await doFetch(this.base + path, {
        method,
        headers,
        body: body === undefined ? undefined : JSON.stringify(body),
        signal: AbortSignal.timeout(this.opts.timeoutMs ?? 30_000),
      });
    } catch (e) {
      throw new BurrowDBError(
        `burrowdb: ${method} ${path} failed: ${e instanceof Error ? e.message : e}`,
        "unreachable",
        0,
      );
    }
    const text = await res.text();
    let json: unknown;
    try {
      json = text ? JSON.parse(text) : {};
    } catch {
      throw new BurrowDBError(
        `burrowdb: ${method} ${path}: response is not JSON`,
        "bad_response",
        res.status,
      );
    }
    if (!res.ok) {
      const err = (json as { error?: { code?: string; message?: string } }).error;
      throw new BurrowDBError(
        err?.message ?? `burrowdb: ${method} ${path} returned ${res.status}`,
        err?.code ?? "http_error",
        res.status,
      );
    }
    return json as R;
  }
}

export class Collection<T extends object = Document> {
  constructor(
    private readonly db: BurrowDB,
    readonly name: string,
  ) {}

  private path(suffix = ""): string {
    return `/collections/${seg(this.name)}${suffix}`;
  }

  /** Inserts a document and returns its _id. */
  async insertOne(doc: WithoutId<T>): Promise<string> {
    const res = await this.db.request<{ id: string }>("POST", this.path("/documents"), doc);
    return res.id;
  }

  /** Inserts documents atomically (all or none) and returns their _ids. */
  async insertMany(docs: WithoutId<T>[]): Promise<string[]> {
    const res = await this.db.request<{ ids: string[] }>("POST", this.path("/documents"), docs);
    return res.ids;
  }

  /** The document with this _id, or null. */
  async findById(id: string): Promise<WithId<T> | null> {
    try {
      return await this.db.request<WithId<T>>("GET", this.path(`/documents/${seg(id)}`));
    } catch (e) {
      if (e instanceof BurrowDBError && e.code === "not_found") return null;
      throw e;
    }
  }

  async find(filter: Filter<T> = {}, options: FindOptions<T> = {}): Promise<WithId<T>[]> {
    const res = await this.db.request<{ documents: WithId<T>[] }>("POST", this.path("/find"), {
      filter,
      ...options,
    });
    return res.documents;
  }

  /** The first match (in sort order if given), or null. */
  async findOne(
    filter: Filter<T> = {},
    options: Omit<FindOptions<T>, "limit"> = {},
  ): Promise<WithId<T> | null> {
    const docs = await this.find(filter, { ...options, limit: 1 });
    return docs[0] ?? null;
  }

  async count(filter: Filter<T> = {}): Promise<number> {
    const res = await this.db.request<{ count: number }>("POST", this.path("/count"), { filter });
    return res.count;
  }

  /** Replaces an existing document; throws BurrowDBError "not_found" if there is none. */
  async replaceOne(id: string, doc: WithoutId<T>): Promise<void> {
    await this.db.request("PUT", this.path(`/documents/${seg(id)}`), doc);
  }

  /** Deletes a document and reports whether it existed. */
  async deleteOne(id: string): Promise<boolean> {
    const res = await this.db.request<{ deleted: boolean }>(
      "DELETE",
      this.path(`/documents/${seg(id)}`),
    );
    return res.deleted;
  }

  async createIndex(field: FieldName<T>): Promise<void> {
    await this.db.request("POST", this.path("/indexes"), { field });
  }

  async dropIndex(field: FieldName<T>): Promise<void> {
    await this.db.request("DELETE", this.path(`/indexes/${seg(field)}`));
  }

  async indexes(): Promise<IndexInfo[]> {
    const res = await this.db.request<{ indexes: IndexInfo[] }>("GET", this.path("/indexes"));
    return res.indexes;
  }

  /** Runs the query and reports the plan it used and the work it did. */
  async explain(filter: Filter<T> = {}, options: FindOptions<T> = {}): Promise<Explain> {
    return this.db.request<Explain>("POST", this.path("/explain"), { filter, ...options });
  }
}
