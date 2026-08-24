import { mkdirSync } from "node:fs";
import { dirname } from "node:path";
import { DatabaseSync } from "node:sqlite";
import type { ConflictRow, MemoryRow, MemoryState } from "./types.ts";

const SCHEMA_VERSION = "1";

// Everything in this file is derived data: f(memory files, dictionary). It can
// be deleted at any time and rebuilt by a full reconcile — no content may exist
// only here (usage heat is the one deliberate exception: losing it degrades
// ranking slightly and nothing else).
const SCHEMA = `
CREATE TABLE IF NOT EXISTS meta(key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS memories(
  id TEXT PRIMARY KEY,
  rel_path TEXT NOT NULL UNIQUE,
  title TEXT NOT NULL DEFAULT '',
  type TEXT NOT NULL,
  state TEXT NOT NULL,
  trust TEXT NOT NULL,
  source TEXT NOT NULL DEFAULT '',
  created TEXT NOT NULL DEFAULT '',
  updated TEXT NOT NULL DEFAULT '',
  review_after TEXT NOT NULL DEFAULT '',
  superseded_by TEXT NOT NULL DEFAULT '',
  content_sha TEXT NOT NULL,
  body_sha TEXT NOT NULL,
  excerpt TEXT NOT NULL DEFAULT '',
  mtime_ns TEXT NOT NULL DEFAULT '0',
  size INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_memories_state ON memories(state);
CREATE TABLE IF NOT EXISTS entities(
  memory_id TEXT NOT NULL,
  entity TEXT NOT NULL,
  PRIMARY KEY(memory_id, entity)
);
CREATE INDEX IF NOT EXISTS idx_entities_entity ON entities(entity);
CREATE TABLE IF NOT EXISTS links(
  from_id TEXT NOT NULL,
  to_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  PRIMARY KEY(from_id, to_id, kind)
);
CREATE TABLE IF NOT EXISTS usage(
  memory_id TEXT PRIMARY KEY,
  recall_count INTEGER NOT NULL DEFAULT 0,
  last_recalled TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS conflicts(
  pair_hash TEXT PRIMARY KEY,
  a_id TEXT NOT NULL,
  b_id TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  detected_at TEXT NOT NULL DEFAULT '',
  resolved_at TEXT NOT NULL DEFAULT '',
  decision TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS unparseable(
  rel_path TEXT PRIMARY KEY,
  error TEXT NOT NULL DEFAULT '',
  mtime_ns TEXT NOT NULL DEFAULT '0',
  size INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS docs(
  rowid INTEGER PRIMARY KEY,
  memory_id TEXT NOT NULL UNIQUE,
  title TEXT NOT NULL DEFAULT '',
  body TEXT NOT NULL DEFAULT '',
  entities_text TEXT NOT NULL DEFAULT ''
);
CREATE VIRTUAL TABLE IF NOT EXISTS fts_word USING fts5(
  title, body, entities_text,
  content='docs', content_rowid='rowid',
  tokenize='unicode61 remove_diacritics 2'
);
CREATE VIRTUAL TABLE IF NOT EXISTS fts_tri USING fts5(
  title, body, entities_text,
  content='docs', content_rowid='rowid',
  tokenize='trigram'
);
CREATE TRIGGER IF NOT EXISTS docs_ai AFTER INSERT ON docs BEGIN
  INSERT INTO fts_word(rowid, title, body, entities_text)
    VALUES (new.rowid, new.title, new.body, new.entities_text);
  INSERT INTO fts_tri(rowid, title, body, entities_text)
    VALUES (new.rowid, new.title, new.body, new.entities_text);
END;
CREATE TRIGGER IF NOT EXISTS docs_ad AFTER DELETE ON docs BEGIN
  INSERT INTO fts_word(fts_word, rowid, title, body, entities_text)
    VALUES ('delete', old.rowid, old.title, old.body, old.entities_text);
  INSERT INTO fts_tri(fts_tri, rowid, title, body, entities_text)
    VALUES ('delete', old.rowid, old.title, old.body, old.entities_text);
END;
CREATE TRIGGER IF NOT EXISTS docs_au AFTER UPDATE ON docs BEGIN
  INSERT INTO fts_word(fts_word, rowid, title, body, entities_text)
    VALUES ('delete', old.rowid, old.title, old.body, old.entities_text);
  INSERT INTO fts_tri(fts_tri, rowid, title, body, entities_text)
    VALUES ('delete', old.rowid, old.title, old.body, old.entities_text);
  INSERT INTO fts_word(rowid, title, body, entities_text)
    VALUES (new.rowid, new.title, new.body, new.entities_text);
  INSERT INTO fts_tri(rowid, title, body, entities_text)
    VALUES (new.rowid, new.title, new.body, new.entities_text);
END;
`;

export interface IndexedFileStat {
  relPath: string;
  mtimeNs: string;
  size: number;
  contentSha: string;
}

export interface UpsertDoc {
  row: MemoryRow;
  body: string;
  entities: string[];
}

interface RawMemoryRow {
  id: string;
  rel_path: string;
  title: string;
  type: string;
  state: string;
  trust: string;
  source: string;
  created: string;
  updated: string;
  review_after: string;
  superseded_by: string;
  content_sha: string;
  body_sha: string;
  excerpt: string;
  mtime_ns: string;
  size: number;
}

function toMemoryRow(raw: RawMemoryRow): MemoryRow {
  return {
    id: raw.id,
    relPath: raw.rel_path,
    title: raw.title,
    type: raw.type as MemoryRow["type"],
    state: raw.state as MemoryRow["state"],
    trust: raw.trust as MemoryRow["trust"],
    source: raw.source,
    created: raw.created,
    updated: raw.updated,
    reviewAfter: raw.review_after,
    supersededBy: raw.superseded_by,
    contentSha: raw.content_sha,
    bodySha: raw.body_sha,
    excerpt: raw.excerpt,
    mtimeNs: raw.mtime_ns,
    size: raw.size,
  };
}

export class DerivedIndex {
  private readonly db: DatabaseSync;

  constructor(dbPath: string) {
    if (dbPath !== ":memory:") mkdirSync(dirname(dbPath), { recursive: true });
    this.db = new DatabaseSync(dbPath);
    this.db.exec("PRAGMA journal_mode = WAL");
    this.db.exec(SCHEMA);
    this.setMeta("schema_version", SCHEMA_VERSION);
  }

  close(): void {
    this.db.close();
  }

  getMeta(key: string): string {
    const row = this.db.prepare("SELECT value FROM meta WHERE key = ?").get(key) as
      | { value: string }
      | undefined;
    return row?.value ?? "";
  }

  setMeta(key: string, value: string): void {
    this.db
      .prepare(
        "INSERT INTO meta(key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value",
      )
      .run(key, value);
  }

  transaction<T>(fn: () => T): T {
    this.db.exec("BEGIN");
    try {
      const result = fn();
      this.db.exec("COMMIT");
      return result;
    } catch (err) {
      this.db.exec("ROLLBACK");
      throw err;
    }
  }

  listFileStats(): Map<string, IndexedFileStat> {
    const rows = this.db
      .prepare("SELECT rel_path, mtime_ns, size, content_sha FROM memories")
      .all() as { rel_path: string; mtime_ns: string; size: number; content_sha: string }[];
    const map = new Map<string, IndexedFileStat>();
    for (const r of rows) {
      map.set(r.rel_path, {
        relPath: r.rel_path,
        mtimeNs: r.mtime_ns,
        size: r.size,
        contentSha: r.content_sha,
      });
    }
    return map;
  }

  listUnparseableStats(): Map<string, { mtimeNs: string; size: number }> {
    const rows = this.db.prepare("SELECT rel_path, mtime_ns, size FROM unparseable").all() as {
      rel_path: string;
      mtime_ns: string;
      size: number;
    }[];
    return new Map(rows.map((r) => [r.rel_path, { mtimeNs: r.mtime_ns, size: r.size }]));
  }

  getByPath(relPath: string): MemoryRow | undefined {
    const raw = this.db.prepare("SELECT * FROM memories WHERE rel_path = ?").get(relPath) as
      | RawMemoryRow
      | undefined;
    return raw ? toMemoryRow(raw) : undefined;
  }

  getById(id: string): MemoryRow | undefined {
    const raw = this.db.prepare("SELECT * FROM memories WHERE id = ?").get(id) as
      | RawMemoryRow
      | undefined;
    return raw ? toMemoryRow(raw) : undefined;
  }

  findByBodySha(bodySha: string): MemoryRow[] {
    const raws = this.db
      .prepare("SELECT * FROM memories WHERE body_sha = ?")
      .all(bodySha) as unknown as RawMemoryRow[];
    return raws.map(toMemoryRow);
  }

  upsert(doc: UpsertDoc): void {
    const r = doc.row;
    this.transaction(() => {
      const previous = this.db
        .prepare("SELECT id FROM memories WHERE rel_path = ?")
        .get(r.relPath) as { id: string } | undefined;
      if (previous && previous.id !== r.id) this.removeById(previous.id);
      this.db
        .prepare(
          `INSERT INTO memories(id, rel_path, title, type, state, trust, source, created, updated,
             review_after, superseded_by, content_sha, body_sha, excerpt, mtime_ns, size)
           VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
           ON CONFLICT(id) DO UPDATE SET
             rel_path = excluded.rel_path, title = excluded.title, type = excluded.type,
             state = excluded.state, trust = excluded.trust, source = excluded.source,
             created = excluded.created, updated = excluded.updated,
             review_after = excluded.review_after, superseded_by = excluded.superseded_by,
             content_sha = excluded.content_sha, body_sha = excluded.body_sha,
             excerpt = excluded.excerpt, mtime_ns = excluded.mtime_ns, size = excluded.size`,
        )
        .run(
          r.id,
          r.relPath,
          r.title,
          r.type,
          r.state,
          r.trust,
          r.source,
          r.created,
          r.updated,
          r.reviewAfter,
          r.supersededBy,
          r.contentSha,
          r.bodySha,
          r.excerpt,
          r.mtimeNs,
          r.size,
        );
      this.db.prepare("DELETE FROM entities WHERE memory_id = ?").run(r.id);
      const insertEntity = this.db.prepare(
        "INSERT OR IGNORE INTO entities(memory_id, entity) VALUES (?, ?)",
      );
      for (const entity of doc.entities) insertEntity.run(r.id, entity);
      const entitiesText = doc.entities.join(" ");
      const existingDoc = this.db.prepare("SELECT rowid FROM docs WHERE memory_id = ?").get(r.id) as
        | { rowid: number }
        | undefined;
      if (existingDoc) {
        this.db
          .prepare("UPDATE docs SET title = ?, body = ?, entities_text = ? WHERE memory_id = ?")
          .run(r.title, doc.body, entitiesText, r.id);
      } else {
        this.db
          .prepare("INSERT INTO docs(memory_id, title, body, entities_text) VALUES (?, ?, ?, ?)")
          .run(r.id, r.title, doc.body, entitiesText);
      }
      this.db.prepare("DELETE FROM unparseable WHERE rel_path = ?").run(r.relPath);
    });
  }

  removeById(id: string): void {
    this.db.prepare("DELETE FROM docs WHERE memory_id = ?").run(id);
    this.db.prepare("DELETE FROM entities WHERE memory_id = ?").run(id);
    this.db.prepare("DELETE FROM usage WHERE memory_id = ?").run(id);
    this.db.prepare("DELETE FROM links WHERE from_id = ? OR to_id = ?").run(id, id);
    this.db.prepare("DELETE FROM memories WHERE id = ?").run(id);
  }

  removeByPath(relPath: string): void {
    const row = this.getByPath(relPath);
    if (row) this.removeById(row.id);
    this.db.prepare("DELETE FROM unparseable WHERE rel_path = ?").run(relPath);
  }

  markUnparseable(relPath: string, error: string, mtimeNs: string, size: number): void {
    const existing = this.getByPath(relPath);
    if (existing) this.removeById(existing.id);
    this.db
      .prepare(
        `INSERT INTO unparseable(rel_path, error, mtime_ns, size) VALUES (?, ?, ?, ?)
         ON CONFLICT(rel_path) DO UPDATE SET error = excluded.error,
           mtime_ns = excluded.mtime_ns, size = excluded.size`,
      )
      .run(relPath, error, mtimeNs, size);
  }

  listUnparseable(): { relPath: string; error: string }[] {
    const rows = this.db
      .prepare("SELECT rel_path, error FROM unparseable ORDER BY rel_path")
      .all() as {
      rel_path: string;
      error: string;
    }[];
    return rows.map((r) => ({ relPath: r.rel_path, error: r.error }));
  }

  searchWord(query: string, limit: number): Map<string, number> {
    return this.searchFts("fts_word", query, limit);
  }

  searchTrigram(query: string, limit: number): Map<string, number> {
    return this.searchFts("fts_tri", query, limit);
  }

  private searchFts(
    table: "fts_word" | "fts_tri",
    query: string,
    limit: number,
  ): Map<string, number> {
    const out = new Map<string, number>();
    const rows = this.db
      .prepare(
        `SELECT d.memory_id AS memory_id, bm25(${table}) AS score
         FROM ${table} JOIN docs d ON d.rowid = ${table}.rowid
         WHERE ${table} MATCH ? ORDER BY score LIMIT ?`,
      )
      .all(query, limit) as { memory_id: string; score: number }[];
    for (const r of rows) {
      // bm25() is smaller-is-better (negative for good matches); flip sign so
      // callers deal in higher-is-better.
      out.set(r.memory_id, Math.max(out.get(r.memory_id) ?? 0, -r.score));
    }
    return out;
  }

  searchLike(token: string, limit: number): string[] {
    const escaped = token.replace(/[\\%_]/g, (c) => `\\${c}`);
    const rows = this.db
      .prepare(
        `SELECT memory_id FROM docs
         WHERE title LIKE ? ESCAPE '\\' OR body LIKE ? ESCAPE '\\' OR entities_text LIKE ? ESCAPE '\\'
         LIMIT ?`,
      )
      .all(`%${escaped}%`, `%${escaped}%`, `%${escaped}%`, limit) as { memory_id: string }[];
    return rows.map((r) => r.memory_id);
  }

  idsByEntities(entities: string[]): Set<string> {
    const out = new Set<string>();
    if (entities.length === 0) return out;
    const placeholders = entities.map(() => "?").join(", ");
    const rows = this.db
      .prepare(`SELECT DISTINCT memory_id FROM entities WHERE entity IN (${placeholders})`)
      .all(...entities) as { memory_id: string }[];
    for (const r of rows) out.add(r.memory_id);
    return out;
  }

  entitiesOf(id: string): string[] {
    const rows = this.db
      .prepare("SELECT entity FROM entities WHERE memory_id = ? ORDER BY entity")
      .all(id) as { entity: string }[];
    return rows.map((r) => r.entity);
  }

  addLink(fromId: string, toId: string, kind: string): void {
    this.db
      .prepare("INSERT OR IGNORE INTO links(from_id, to_id, kind) VALUES (?, ?, ?)")
      .run(fromId, toId, kind);
  }

  linksOf(id: string): { fromId: string; toId: string; kind: string }[] {
    const rows = this.db
      .prepare("SELECT from_id, to_id, kind FROM links WHERE from_id = ? OR to_id = ?")
      .all(id, id) as { from_id: string; to_id: string; kind: string }[];
    return rows.map((r) => ({ fromId: r.from_id, toId: r.to_id, kind: r.kind }));
  }

  recordRecall(ids: string[], when: string): void {
    const stmt = this.db.prepare(
      `INSERT INTO usage(memory_id, recall_count, last_recalled) VALUES (?, 1, ?)
       ON CONFLICT(memory_id) DO UPDATE SET recall_count = recall_count + 1, last_recalled = excluded.last_recalled`,
    );
    for (const id of ids) stmt.run(id, when);
  }

  usageOf(id: string): { recallCount: number; lastRecalled: string } {
    const row = this.db
      .prepare("SELECT recall_count, last_recalled FROM usage WHERE memory_id = ?")
      .get(id) as { recall_count: number; last_recalled: string } | undefined;
    return { recallCount: row?.recall_count ?? 0, lastRecalled: row?.last_recalled ?? "" };
  }

  upsertConflict(row: ConflictRow): boolean {
    const existing = this.db
      .prepare("SELECT pair_hash FROM conflicts WHERE pair_hash = ?")
      .get(row.pairHash);
    if (existing) return false;
    this.db
      .prepare(
        "INSERT INTO conflicts(pair_hash, a_id, b_id, status, detected_at, resolved_at, decision) VALUES (?, ?, ?, ?, ?, ?, ?)",
      )
      .run(
        row.pairHash,
        row.aId,
        row.bId,
        row.status,
        row.detectedAt,
        row.resolvedAt,
        row.decision,
      );
    return true;
  }

  getConflict(pairHash: string): ConflictRow | undefined {
    const r = this.db.prepare("SELECT * FROM conflicts WHERE pair_hash = ?").get(pairHash) as
      | {
          pair_hash: string;
          a_id: string;
          b_id: string;
          status: string;
          detected_at: string;
          resolved_at: string;
          decision: string;
        }
      | undefined;
    if (!r) return undefined;
    return {
      pairHash: r.pair_hash,
      aId: r.a_id,
      bId: r.b_id,
      status: r.status as ConflictRow["status"],
      detectedAt: r.detected_at,
      resolvedAt: r.resolved_at,
      decision: r.decision,
    };
  }

  resolveConflictsBetween(aId: string, bId: string, decision: string, when: string): number {
    const result = this.db
      .prepare(
        `UPDATE conflicts SET status = 'resolved', decision = ?, resolved_at = ?
         WHERE status = 'pending' AND ((a_id = ? AND b_id = ?) OR (a_id = ? AND b_id = ?))`,
      )
      .run(decision, when, aId, bId, bId, aId);
    return Number(result.changes);
  }

  pendingConflicts(): ConflictRow[] {
    const rows = this.db
      .prepare("SELECT * FROM conflicts WHERE status = 'pending' ORDER BY detected_at")
      .all() as {
      pair_hash: string;
      a_id: string;
      b_id: string;
      status: string;
      detected_at: string;
      resolved_at: string;
      decision: string;
    }[];
    return rows.map((r) => ({
      pairHash: r.pair_hash,
      aId: r.a_id,
      bId: r.b_id,
      status: r.status as ConflictRow["status"],
      detectedAt: r.detected_at,
      resolvedAt: r.resolved_at,
      decision: r.decision,
    }));
  }

  countByColumn(column: "state" | "type" | "trust"): Record<string, number> {
    const rows = this.db
      .prepare(`SELECT ${column} AS k, COUNT(*) AS n FROM memories GROUP BY ${column}`)
      .all() as { k: string; n: number }[];
    const out: Record<string, number> = {};
    for (const r of rows) out[r.k] = r.n;
    return out;
  }

  totalCount(): number {
    const row = this.db.prepare("SELECT COUNT(*) AS n FROM memories").get() as { n: number };
    return row.n;
  }

  listByState(state: MemoryState): MemoryRow[] {
    const raws = this.db
      .prepare("SELECT * FROM memories WHERE state = ? ORDER BY updated DESC")
      .all(state) as unknown as RawMemoryRow[];
    return raws.map(toMemoryRow);
  }

  reviewDue(today: string): MemoryRow[] {
    const raws = this.db
      .prepare(
        "SELECT * FROM memories WHERE review_after != '' AND review_after <= ? AND state = 'active' ORDER BY review_after",
      )
      .all(today) as unknown as RawMemoryRow[];
    return raws.map(toMemoryRow);
  }
}
