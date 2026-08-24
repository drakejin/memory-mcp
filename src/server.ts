import { isAbsolute, join, relative, resolve } from "node:path";
import type { McpServer } from "@modelcontextprotocol/sdk/server/mcp.js";
import { z } from "zod";
import { status } from "./maintenance.ts";
import { recall } from "./search.ts";
import { type MemoryStore, openStore } from "./store.ts";
import { MEMORY_STATES, MEMORY_TYPES, type MemoryType, StoreError, TRUST_LEVELS } from "./types.ts";

type ToolResult = {
  content: { type: "text"; text: string }[];
  isError?: boolean;
};

function ok(payload: unknown): ToolResult {
  return { content: [{ type: "text", text: JSON.stringify(payload, null, 2) }] };
}

function fail(err: unknown): ToolResult {
  const message =
    err instanceof StoreError
      ? { error: err.code, message: err.message }
      : { error: "internal", message: (err as Error).message };
  return { content: [{ type: "text", text: JSON.stringify(message, null, 2) }], isError: true };
}

function rowSummary(store: MemoryStore, id: string) {
  const row = store.index.getById(id);
  if (!row) return { id };
  return {
    id: row.id,
    path: store.absPathOf(row),
    title: row.title,
    type: row.type,
    state: row.state,
    trust: row.trust,
    entities: store.index.entitiesOf(row.id),
    supersededBy: row.supersededBy || undefined,
    updated: row.updated,
  };
}

function coverageOf(store: MemoryStore) {
  return {
    lastReconcile: store.index.getMeta("last_reconcile"),
    indexed: store.index.totalCount(),
    unparseable: store.index.listUnparseable().length,
    note: "absence from the index is not proof a memory does not exist — check unparseable via memory_status",
  };
}

const storeParam = z
  .string()
  .optional()
  .describe("Absolute path of the memory store directory. Omit for the default store.");

export function registerTools(server: McpServer): void {
  server.registerTool(
    "memory_remember",
    {
      title: "Remember",
      description:
        "Create a memory (markdown original + derived index). Returns possible_conflicts with " +
        "judgment material (shared entities, age gap, latest_wins_eligible) — the caller judges: " +
        "supersede, keep both, or correct itself. Nothing is ever overwritten automatically. " +
        "Identical content is idempotent (returns the existing id).",
      inputSchema: {
        store: storeParam,
        content: z.string().describe("Markdown body. First heading (or line) becomes the title."),
        type: z.enum(MEMORY_TYPES).default("fact"),
        trust: z
          .enum(TRUST_LEVELS)
          .default("agent-inferred")
          .describe("user-stated > agent-inferred > imported; user corrections outrank inference"),
        entities: z
          .array(z.string())
          .default([])
          .describe("Entity keys; normalized via dictionary.md when present"),
        source: z
          .string()
          .default("")
          .describe("Provenance: where this came from (session, doc, url)"),
        supersedes: z
          .array(z.string())
          .default([])
          .describe("Ids this memory replaces; they get superseded_by and sink to archived"),
        review_after: z
          .string()
          .default("")
          .describe("ISO date to resurface this memory for review (not a delete TTL)"),
      },
    },
    async (args) => {
      try {
        const store = openStore(args.store);
        store.reconcile();
        const result = store.remember({
          content: args.content,
          type: args.type,
          trust: args.trust,
          entities: args.entities,
          source: args.source,
          supersedes: args.supersedes,
          reviewAfter: args.review_after,
        });
        if (result.duplicateOf) {
          return ok({
            duplicate: true,
            memory: rowSummary(store, result.duplicateOf),
            note: "identical body already remembered — no new memory created",
          });
        }
        const conflicts = result.conflicts;
        return ok({
          memory: rowSummary(store, result.row.id),
          superseded: result.supersededIds,
          unknown_entities: result.unknownEntities.length > 0 ? result.unknownEntities : undefined,
          possible_conflicts: conflicts.length > 0 ? conflicts : undefined,
          guidance:
            conflicts.length > 0
              ? "These memories may contradict the new one. Decide: memory_update(action=supersede) to retire one side, action=resolve_conflict decision=keep-both if they coexist, or ask the user. latest_wins_eligible=true means the age gap exceeds the ambiguity window and superseding the older side is usually safe."
              : undefined,
        });
      } catch (err) {
        return fail(err);
      }
    },
  );

  server.registerTool(
    "memory_recall",
    {
      title: "Recall",
      description:
        "Hybrid lexical search (word/trigram FTS + CJK-aware fallbacks). Returns paths, excerpts, " +
        "and score components — read the file (or memory_read) for full content; nothing is auto-injected. " +
        "Ranking: text * trust * state * (1 + freshness + heat); old unused memories sink, never vanish.",
      inputSchema: {
        store: storeParam,
        query: z
          .string()
          .default("")
          .describe("Free-text query; Korean particles/endings matched by prefix+trigram"),
        entities: z.array(z.string()).default([]).describe("Filter/boost by entity keys (any-of)"),
        types: z.array(z.enum(MEMORY_TYPES)).default([]),
        include_archived: z.boolean().default(false),
        include_deprecated: z.boolean().default(false),
        limit: z.number().int().min(1).max(50).default(8),
      },
    },
    async (args) => {
      try {
        const store = openStore(args.store);
        store.reconcile();
        const hits = recall(store, {
          query: args.query,
          entities: args.entities,
          types: args.types as MemoryType[],
          includeArchived: args.include_archived,
          includeDeprecated: args.include_deprecated,
          limit: args.limit,
        });
        store.index.recordRecall(
          hits.map((h) => h.row.id),
          new Date().toISOString(),
        );
        return ok({
          results: hits.map((h) => ({
            id: h.row.id,
            path: store.absPathOf(h.row),
            title: h.row.title,
            excerpt: h.row.excerpt,
            type: h.row.type,
            state: h.row.state,
            trust: h.row.trust,
            updated: h.row.updated,
            supersededBy: h.row.supersededBy || undefined,
            score: Math.round(h.score * 1000) / 1000,
            components: h.components,
            matchedEntities: h.matchedEntities.length > 0 ? h.matchedEntities : undefined,
          })),
          coverage: coverageOf(store),
        });
      } catch (err) {
        return fail(err);
      }
    },
  );

  server.registerTool(
    "memory_read",
    {
      title: "Read memory",
      description:
        "Full content plus provenance: frontmatter, supersede chain in both directions, conflicts, usage.",
      inputSchema: {
        store: storeParam,
        id: z.string().optional().describe("Memory id (ULID)"),
        path: z
          .string()
          .optional()
          .describe("Path of a memory file inside the store (instead of id)"),
      },
    },
    async (args) => {
      try {
        const store = openStore(args.store);
        store.reconcile();
        let id = args.id;
        if (!id && args.path) {
          const abs = isAbsolute(args.path)
            ? resolve(args.path)
            : resolve(join(store.storeDir, args.path));
          const rel = relative(store.storeDir, abs);
          if (rel.startsWith("..")) {
            throw new StoreError("containment", `path is outside the store: ${args.path}`);
          }
          const row = store.index.getByPath(rel);
          if (!row) throw new StoreError("not_found", `no indexed memory at: ${rel}`);
          id = row.id;
        }
        if (!id) throw new StoreError("bad_request", "either id or path is required");
        const { row, file } = store.readMemoryFileById(id);
        const chain = store.chainOf(id);
        const usage = store.index.usageOf(id);
        const conflicts = store.index
          .pendingConflicts()
          .filter((c) => c.aId === id || c.bId === id)
          .map((c) => ({ withId: c.aId === id ? c.bId : c.aId, detectedAt: c.detectedAt }));
        return ok({
          memory: rowSummary(store, id),
          frontmatter: {
            ...file.front,
            extra: Object.keys(file.front.extra).length > 0 ? file.front.extra : undefined,
          },
          body: file.body,
          chain,
          pendingConflicts: conflicts.length > 0 ? conflicts : undefined,
          usage,
          path: store.absPathOf(row),
        });
      } catch (err) {
        return fail(err);
      }
    },
  );

  server.registerTool(
    "memory_update",
    {
      title: "Update memory",
      description:
        "edit: revise wording/metadata in place (state preserved — meaning-changing revisions should " +
        "use supersede instead). supersede: id wins over other_id (non-destructive version chain). " +
        "set_state: explicit lifecycle transition. link: relate two memories. " +
        "resolve_conflict: record a keep-both judgment for a surfaced pair.",
      inputSchema: {
        store: storeParam,
        id: z.string().describe("Subject memory id"),
        action: z.enum(["edit", "supersede", "set_state", "link", "resolve_conflict"]),
        content: z.string().optional().describe("edit: new markdown body"),
        entities: z.array(z.string()).optional().describe("edit: replace entity list"),
        source: z.string().optional().describe("edit: replace provenance"),
        type: z.enum(MEMORY_TYPES).optional().describe("edit: change type"),
        review_after: z.string().optional().describe("edit: change review date ('' clears)"),
        state: z.enum(MEMORY_STATES).optional().describe("set_state: target state"),
        reason: z.string().optional().describe("set_state->deprecated: why it is wrong (required)"),
        other_id: z
          .string()
          .optional()
          .describe("supersede/link/resolve_conflict: the other memory"),
        link_kind: z.string().default("relates").describe("link: relation kind"),
      },
    },
    async (args) => {
      try {
        const store = openStore(args.store);
        store.reconcile();
        switch (args.action) {
          case "edit": {
            const { row, unknownEntities } = store.edit(args.id, {
              content: args.content,
              entities: args.entities,
              source: args.source,
              type: args.type,
              reviewAfter: args.review_after,
            });
            return ok({
              memory: rowSummary(store, row.id),
              unknown_entities: unknownEntities.length > 0 ? unknownEntities : undefined,
            });
          }
          case "supersede": {
            if (!args.other_id)
              throw new StoreError("bad_request", "supersede requires other_id (the loser)");
            store.applySupersede(args.id, args.other_id);
            return ok({
              winner: rowSummary(store, args.id),
              superseded: rowSummary(store, args.other_id),
            });
          }
          case "set_state": {
            if (!args.state) throw new StoreError("bad_request", "set_state requires state");
            const row = store.setState(args.id, args.state, args.reason ?? "");
            return ok({ memory: rowSummary(store, row.id) });
          }
          case "link": {
            if (!args.other_id) throw new StoreError("bad_request", "link requires other_id");
            if (!store.index.getById(args.other_id)) {
              throw new StoreError("not_found", `memory not found: ${args.other_id}`);
            }
            store.index.addLink(args.id, args.other_id, args.link_kind);
            return ok({ linked: [args.id, args.other_id], kind: args.link_kind });
          }
          case "resolve_conflict": {
            if (!args.other_id)
              throw new StoreError("bad_request", "resolve_conflict requires other_id");
            const changed = store.index.resolveConflictsBetween(
              args.id,
              args.other_id,
              "keep-both",
              new Date().toISOString(),
            );
            return ok({
              resolved: changed,
              note:
                changed === 0
                  ? "no pending conflict between these ids — nothing recorded"
                  : "pair recorded as keep-both; it will not be surfaced again",
            });
          }
        }
      } catch (err) {
        return fail(err);
      }
    },
  );

  server.registerTool(
    "memory_forget",
    {
      title: "Forget (state transition, never silent deletion)",
      description:
        "archive: sink from default recall (recoverable). deprecate: judged wrong — requires a reason, " +
        "preserved for opt-in reads. purge: delete the file — only from archived/deprecated and only with " +
        "confirm=true; if the store is a git repo, history is the backstop.",
      inputSchema: {
        store: storeParam,
        id: z.string(),
        mode: z.enum(["archive", "deprecate", "purge"]).default("archive"),
        reason: z.string().default("").describe("deprecate: why this memory is wrong (required)"),
        confirm: z.boolean().default(false).describe("purge: must be true"),
      },
    },
    async (args) => {
      try {
        const store = openStore(args.store);
        store.reconcile();
        if (args.mode === "archive") {
          const row = store.setState(args.id, "archived");
          return ok({ memory: rowSummary(store, row.id) });
        }
        if (args.mode === "deprecate") {
          const row = store.setState(args.id, "deprecated", args.reason);
          return ok({ memory: rowSummary(store, row.id) });
        }
        if (!args.confirm) {
          throw new StoreError(
            "confirm_required",
            "purge deletes the file — pass confirm=true to proceed",
          );
        }
        const { removedPath } = store.purge(args.id);
        return ok({ purged: args.id, removedPath });
      } catch (err) {
        return fail(err);
      }
    },
  );

  server.registerTool(
    "memory_status",
    {
      title: "Status",
      description:
        "The honesty surface: counts by state/type/trust, pending conflict judgments, review-due memories, " +
        "distillation (compaction) candidates, unparseable files, index freshness, dictionary state.",
      inputSchema: { store: storeParam },
    },
    async (args) => {
      try {
        const store = openStore(args.store);
        const report = store.reconcile();
        return ok(status(store, report));
      } catch (err) {
        return fail(err);
      }
    },
  );

  server.registerTool(
    "memory_reindex",
    {
      title: "Reindex",
      description:
        "rebuild (default): drop all derivatives and reconstruct them from the memory files — originals own " +
        "the content, so this is disaster recovery (only usage heat is lost). verify: full re-hash audit that " +
        "ignores the stat gate and reports drift (files whose content no longer matches the index).",
      inputSchema: {
        store: storeParam,
        verify: z.boolean().default(false),
      },
    },
    async (args) => {
      try {
        const store = openStore(args.store);
        if (!args.verify) {
          const report = store.rebuildDerived();
          return ok({
            mode: "rebuild",
            report,
            note: "usage heat reset; everything else rebuilt from files",
          });
        }
        const before = store.index.listFileStats();
        const report = store.reconcile(true);
        const after = store.index.listFileStats();
        const drift: string[] = [];
        for (const [relPath, stat] of after) {
          const prev = before.get(relPath);
          if (!prev || prev.contentSha !== stat.contentSha) drift.push(relPath);
        }
        for (const relPath of before.keys()) {
          if (!after.has(relPath)) drift.push(`${relPath} (removed)`);
        }
        return ok({ mode: "verify", report, drift });
      } catch (err) {
        return fail(err);
      }
    },
  );
}
