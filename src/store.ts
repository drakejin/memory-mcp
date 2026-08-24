import { createHash } from "node:crypto";
import {
  existsSync,
  mkdirSync,
  readdirSync,
  readFileSync,
  rmSync,
  statSync,
  writeFileSync,
} from "node:fs";
import { homedir } from "node:os";
import { isAbsolute, join, relative, resolve, sep } from "node:path";
import { type ConflictCandidate, detectConflicts } from "./conflict.ts";
import { DerivedIndex } from "./db.ts";
import {
  type Dictionary,
  EMPTY_DICTIONARY,
  normalizeEntities,
  parseDictionary,
} from "./dictionary.ts";
import { deriveExcerpt, deriveTitle, parseMemoryFile, serializeMemoryFile } from "./frontmatter.ts";
import { assertTransition, canPurge } from "./lifecycle.ts";
import {
  type MemoryFile,
  type MemoryRow,
  type MemoryState,
  type MemoryType,
  type ReconcileReport,
  StoreError,
  type TrustLevel,
} from "./types.ts";
import { isUlid, ulid } from "./ulid.ts";

const RECONCILE_DEBOUNCE_MS = 2_000;

export function defaultStoreDir(): string {
  const root = process.env.MEMORY_MCP_ROOT;
  if (root && root.trim() !== "") return resolve(root);
  return join(homedir(), ".memory-mcp", "default");
}

export function resolveStoreDir(storeParam?: string): string {
  if (storeParam === undefined || storeParam.trim() === "") return defaultStoreDir();
  if (!isAbsolute(storeParam)) {
    throw new StoreError("invalid_store", `store must be an absolute path: ${storeParam}`);
  }
  return resolve(storeParam);
}

function sha256(text: string): string {
  return createHash("sha256").update(text).digest("hex");
}

function nowIso(): string {
  return new Date().toISOString().replace(/\.\d{3}Z$/, "Z");
}

export interface RememberInput {
  content: string;
  type: MemoryType;
  trust: TrustLevel;
  entities: string[];
  source: string;
  supersedes: string[];
  reviewAfter: string;
}

export interface RememberResult {
  row: MemoryRow;
  duplicateOf?: string;
  unknownEntities: string[];
  supersededIds: string[];
  /** Judgment material — the write path cannot skip the conflict gate. */
  conflicts: ConflictCandidate[];
}

export class MemoryStore {
  readonly storeDir: string;
  readonly memoriesDir: string;
  index: DerivedIndex;
  dictionary: Dictionary = EMPTY_DICTIONARY;
  private lastReconcileAt = 0;
  private lastReport: ReconcileReport = {
    scanned: 0,
    indexed: 0,
    removed: 0,
    promoted: [],
    unparseable: 0,
    skipped: true,
  };

  constructor(storeDir: string) {
    this.storeDir = storeDir;
    this.memoriesDir = join(storeDir, "memories");
    mkdirSync(this.memoriesDir, { recursive: true });
    this.index = new DerivedIndex(join(storeDir, ".derived", "index.db"));
  }

  close(): void {
    this.index.close();
  }

  /**
   * Disaster recovery in one call: derivatives own no content, so dropping
   * .derived/ and re-scanning the files rebuilds everything except usage heat.
   */
  rebuildDerived(): ReconcileReport {
    this.index.close();
    rmSync(join(this.storeDir, ".derived"), { recursive: true, force: true });
    this.index = new DerivedIndex(join(this.storeDir, ".derived", "index.db"));
    this.lastReconcileAt = 0;
    return this.reconcile(true);
  }

  // ---------------------------------------------------------------- reconcile

  /**
   * Call-time lazy reconcile: stat-gate (mtime+size) -> sha256 -> partial
   * re-index; rows whose file vanished are removed. Events do not exist here —
   * every tool call converges the derivatives before acting. A dictionary
   * content-hash change forces a full re-parse (entity normalization is
   * derived data keyed by dictionary version).
   */
  reconcile(force = false): ReconcileReport {
    const now = Date.now();
    if (!force && now - this.lastReconcileAt < RECONCILE_DEBOUNCE_MS) {
      return { ...this.lastReport, skipped: true };
    }
    this.lastReconcileAt = now;

    const dictionaryChanged = this.loadDictionary();
    const indexed = this.index.listFileStats();
    const unparseable = this.index.listUnparseableStats();
    const seen = new Set<string>();
    const report: ReconcileReport = {
      scanned: 0,
      indexed: 0,
      removed: 0,
      promoted: [],
      unparseable: 0,
      skipped: false,
    };

    for (const relPath of this.listMemoryFiles()) {
      report.scanned++;
      seen.add(relPath);
      const absPath = join(this.storeDir, relPath);
      let mtimeNs: string;
      let size: number;
      try {
        const stat = statSync(absPath, { bigint: true });
        mtimeNs = String(stat.mtimeNs);
        size = Number(stat.size);
      } catch {
        continue;
      }
      const known = indexed.get(relPath);
      const knownBad = unparseable.get(relPath);
      const statUnchanged =
        (known && known.mtimeNs === mtimeNs && known.size === size) ||
        (knownBad && knownBad.mtimeNs === mtimeNs && knownBad.size === size);
      if (!force && !dictionaryChanged && statUnchanged) continue;

      this.reindexFile(relPath, mtimeNs, size, report);
      report.indexed++;
    }

    for (const relPath of indexed.keys()) {
      if (!seen.has(relPath)) {
        this.index.removeByPath(relPath);
        report.removed++;
      }
    }
    for (const relPath of unparseable.keys()) {
      if (!seen.has(relPath)) this.index.removeByPath(relPath);
    }

    report.unparseable = this.index.listUnparseable().length;
    this.index.setMeta("last_reconcile", nowIso());
    this.lastReport = report;
    return report;
  }

  private reindexFile(
    relPath: string,
    mtimeNs: string,
    size: number,
    report: ReconcileReport,
  ): void {
    const absPath = join(this.storeDir, relPath);
    let text: string;
    try {
      text = readFileSync(absPath, "utf8");
    } catch (err) {
      this.index.markUnparseable(relPath, `read failed: ${(err as Error).message}`, mtimeNs, size);
      return;
    }
    let file: MemoryFile;
    try {
      file = parseMemoryFile(text);
    } catch (err) {
      this.index.markUnparseable(relPath, (err as Error).message, mtimeNs, size);
      return;
    }

    const bodySha = sha256(file.body);
    const previous = this.index.getByPath(relPath);
    // AX lifecycle transition (3): a hand edit to an archived memory's *body*
    // expresses fresh intent — promote back to active. Tool-driven writes sync
    // the index in the same call, so they never trip this path; frontmatter-only
    // writes (e.g. the system recording superseded_by) keep body_sha stable.
    if (
      previous &&
      previous.state === "archived" &&
      file.front.state === "archived" &&
      previous.bodySha !== bodySha
    ) {
      file.front.state = "active";
      file.front.updated = nowIso();
      const rewritten = serializeMemoryFile(file);
      writeFileSync(absPath, rewritten, "utf8");
      const stat = statSync(absPath, { bigint: true });
      this.upsertFromFile(relPath, file, String(stat.mtimeNs), Number(stat.size));
      report.promoted.push(file.front.id);
      return;
    }

    this.upsertFromFile(relPath, file, mtimeNs, size);
  }

  private upsertFromFile(relPath: string, file: MemoryFile, mtimeNs: string, size: number): void {
    const normalized = normalizeEntities(this.dictionary, file.front.entities);
    const content = serializeMemoryFile(file);
    this.index.upsert({
      row: {
        id: file.front.id,
        relPath,
        title: deriveTitle(file.body),
        type: file.front.type,
        state: file.front.state,
        trust: file.front.trust,
        source: file.front.source,
        created: file.front.created,
        updated: file.front.updated,
        reviewAfter: file.front.reviewAfter,
        supersededBy: file.front.supersededBy,
        contentSha: sha256(content),
        bodySha: sha256(file.body),
        excerpt: deriveExcerpt(file.body),
        mtimeNs,
        size,
      },
      body: file.body,
      entities: normalized.entities,
    });
    for (const target of file.front.supersedes) {
      this.index.addLink(file.front.id, target, "supersedes");
    }
  }

  private loadDictionary(): boolean {
    const path = join(this.storeDir, "dictionary.md");
    let next: Dictionary = EMPTY_DICTIONARY;
    if (existsSync(path)) {
      next = parseDictionary(readFileSync(path, "utf8"));
    }
    const previousVersion = this.index.getMeta("dictionary_version");
    this.dictionary = next;
    if (previousVersion !== next.version) {
      this.index.setMeta("dictionary_version", next.version);
      return previousVersion !== "" || next.version !== "";
    }
    return false;
  }

  private listMemoryFiles(): string[] {
    const out: string[] = [];
    const entries = readdirSync(this.memoriesDir, { recursive: true }) as string[];
    for (const entry of entries) {
      const name = String(entry);
      if (name.endsWith(".md")) out.push(join("memories", name));
    }
    return out.sort();
  }

  // ----------------------------------------------------------------- file io

  absPathOf(row: MemoryRow): string {
    return join(this.storeDir, row.relPath);
  }

  readMemoryFileById(id: string): { row: MemoryRow; file: MemoryFile } {
    if (!isUlid(id)) throw new StoreError("invalid_id", `not a memory id: ${id}`);
    const row = this.index.getById(id);
    if (!row) throw new StoreError("not_found", `memory not found: ${id}`);
    const absPath = this.absPathOf(row);
    const contained = relative(this.storeDir, resolve(absPath));
    if (contained.startsWith("..") || contained.split(sep)[0] !== "memories") {
      throw new StoreError("containment", `path escapes store: ${row.relPath}`);
    }
    const file = parseMemoryFile(readFileSync(absPath, "utf8"));
    return { row, file };
  }

  /** Serialize, write, stat, and sync the index row in one step. */
  writeMemoryFile(relPath: string, file: MemoryFile): MemoryRow {
    const absPath = join(this.storeDir, relPath);
    writeFileSync(absPath, serializeMemoryFile(file), "utf8");
    const stat = statSync(absPath, { bigint: true });
    this.upsertFromFile(relPath, file, String(stat.mtimeNs), Number(stat.size));
    const row = this.index.getById(file.front.id);
    if (!row) throw new StoreError("index_sync", `index row missing after write: ${file.front.id}`);
    return row;
  }

  // ---------------------------------------------------------------- remember

  remember(input: RememberInput): RememberResult {
    const raw = input.content.trim();
    if (raw === "") throw new StoreError("empty_content", "content must not be empty");
    const body = raw.startsWith("#") ? `${raw}\n` : `# ${deriveTitle(raw)}\n\n${raw}\n`;
    const bodySha = sha256(body);

    // Idempotent writes: an identical body that is already remembered (and not
    // judged wrong) returns the existing memory instead of minting a twin.
    const twins = this.index.findByBodySha(bodySha).filter((m) => m.state !== "deprecated");
    if (twins.length > 0 && input.supersedes.length === 0) {
      const existing = twins[0] as MemoryRow;
      return {
        row: existing,
        duplicateOf: existing.id,
        unknownEntities: [],
        supersededIds: [],
        conflicts: [],
      };
    }

    const normalized = normalizeEntities(this.dictionary, input.entities);
    const id = ulid();
    const when = nowIso();
    const file: MemoryFile = {
      front: {
        id,
        type: input.type,
        state: "active",
        trust: input.trust,
        entities: normalized.entities,
        supersedes: [],
        supersededBy: "",
        source: input.source,
        created: when,
        updated: when,
        reviewAfter: input.reviewAfter,
        deprecatedReason: "",
        extra: {},
      },
      body,
    };

    const supersededIds: string[] = [];
    for (const target of input.supersedes) {
      const targetRow = this.index.getById(target);
      if (!targetRow) throw new StoreError("not_found", `supersedes target not found: ${target}`);
      supersededIds.push(target);
    }
    file.front.supersedes = supersededIds;

    const row = this.writeMemoryFile(join("memories", `${id}.md`), file);
    for (const target of supersededIds) this.applySupersede(id, target);
    const finalRow = this.index.getById(id) ?? row;
    return {
      row: finalRow,
      unknownEntities: normalized.unknown,
      supersededIds,
      conflicts: detectConflicts(this, finalRow, normalized.entities),
    };
  }

  /**
   * Non-destructive revision: the loser records superseded_by and, if active,
   * moves to the archived buffer. Nothing is deleted; the chain stays walkable
   * in both directions.
   */
  applySupersede(winnerId: string, loserId: string): void {
    if (winnerId === loserId) throw new StoreError("invalid_supersede", "cannot supersede itself");
    const winner = this.index.getById(winnerId);
    if (!winner) throw new StoreError("not_found", `memory not found: ${winnerId}`);
    const { row, file } = this.readMemoryFileById(loserId);
    file.front.supersededBy = winnerId;
    if (file.front.state === "active") file.front.state = "archived";
    file.front.updated = nowIso();
    this.writeMemoryFile(row.relPath, file);
    this.index.addLink(winnerId, loserId, "supersedes");
    this.index.resolveConflictsBetween(winnerId, loserId, `superseded:${winnerId}`, nowIso());

    const winnerFile = this.readMemoryFileById(winnerId);
    if (!winnerFile.file.front.supersedes.includes(loserId)) {
      winnerFile.file.front.supersedes.push(loserId);
      this.writeMemoryFile(winnerFile.row.relPath, winnerFile.file);
    }
  }

  // ------------------------------------------------------------- transitions

  setState(id: string, to: MemoryState, reason = ""): MemoryRow {
    const { row, file } = this.readMemoryFileById(id);
    assertTransition(file.front.state, to);
    file.front.state = to;
    file.front.updated = nowIso();
    if (to === "deprecated") {
      if (reason.trim() === "") {
        throw new StoreError("reason_required", "deprecating requires a reason — why is it wrong?");
      }
      file.front.deprecatedReason = reason.trim();
    }
    if (to === "active") file.front.deprecatedReason = "";
    return this.writeMemoryFile(row.relPath, file);
  }

  purge(id: string): { removedPath: string } {
    const { row, file } = this.readMemoryFileById(id);
    if (!canPurge(file.front.state)) {
      throw new StoreError(
        "purge_requires_buffer",
        `purge is only allowed from archived/deprecated (current: ${file.front.state}) — archive or deprecate first`,
      );
    }
    rmSync(this.absPathOf(row));
    this.index.removeByPath(row.relPath);
    return { removedPath: row.relPath };
  }

  edit(
    id: string,
    patch: {
      content?: string;
      entities?: string[];
      source?: string;
      type?: MemoryType;
      reviewAfter?: string;
    },
  ): { row: MemoryRow; unknownEntities: string[] } {
    const { row, file } = this.readMemoryFileById(id);
    let unknownEntities: string[] = [];
    if (patch.content !== undefined) {
      const body = patch.content.trim();
      if (body === "") throw new StoreError("empty_content", "content must not be empty");
      file.body = body.startsWith("#") ? `${body}\n` : `# ${deriveTitle(body)}\n\n${body}\n`;
    }
    if (patch.entities !== undefined) {
      const normalized = normalizeEntities(this.dictionary, patch.entities);
      file.front.entities = normalized.entities;
      unknownEntities = normalized.unknown;
    }
    if (patch.source !== undefined) file.front.source = patch.source;
    if (patch.type !== undefined) file.front.type = patch.type;
    if (patch.reviewAfter !== undefined) file.front.reviewAfter = patch.reviewAfter;
    file.front.updated = nowIso();
    return { row: this.writeMemoryFile(row.relPath, file), unknownEntities };
  }

  chainOf(id: string): { supersedes: string[]; supersededBy: string[] } {
    const walk = (start: string, direction: "up" | "down"): string[] => {
      const out: string[] = [];
      let cursor = start;
      for (let depth = 0; depth < 10; depth++) {
        const row = this.index.getById(cursor);
        if (!row) break;
        if (direction === "up") {
          if (row.supersededBy === "") break;
          out.push(row.supersededBy);
          cursor = row.supersededBy;
        } else {
          const links = this.index
            .linksOf(cursor)
            .filter((l) => l.kind === "supersedes" && l.fromId === cursor);
          const next = links[0]?.toId;
          if (!next) break;
          out.push(next);
          cursor = next;
        }
      }
      return out;
    };
    return { supersededBy: walk(id, "up"), supersedes: walk(id, "down") };
  }
}

// Store instances are cached per resolved path so every tool call shares the
// same SQLite handle and reconcile debounce window.
const storeCache = new Map<string, MemoryStore>();

export function openStore(storeParam?: string): MemoryStore {
  const dir = resolveStoreDir(storeParam);
  let store = storeCache.get(dir);
  if (!store) {
    store = new MemoryStore(dir);
    storeCache.set(dir, store);
  }
  return store;
}

export function closeAllStores(): void {
  for (const store of storeCache.values()) store.close();
  storeCache.clear();
}
