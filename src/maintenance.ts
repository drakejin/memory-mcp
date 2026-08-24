import type { MemoryStore } from "./store.ts";
import { COMPACTION_AGE_DAYS, type ReconcileReport } from "./types.ts";

// memory_status is the honesty surface: counts, pending judgments, review-due
// memories, distillation candidates, unparseable files, index freshness. The
// absence of an entry is never a completeness guarantee — the caller is told
// what the index could not see (unparseable) and when it last converged.

export interface StatusReport {
  store: string;
  counts: {
    total: number;
    byState: Record<string, number>;
    byType: Record<string, number>;
    byTrust: Record<string, number>;
  };
  pendingConflicts: {
    aId: string;
    bId: string;
    aTitle: string;
    bTitle: string;
    detectedAt: string;
  }[];
  reviewDue: { id: string; title: string; reviewAfter: string }[];
  compactionCandidates: {
    id: string;
    title: string;
    type: string;
    updated: string;
    recallCount: number;
    hint: string;
  }[];
  unparseable: { relPath: string; error: string }[];
  index: {
    lastReconcile: string;
    lastScan: ReconcileReport;
    dictionary: { present: boolean; version: string; entries: number };
  };
  notes: string[];
}

export function status(store: MemoryStore, lastScan: ReconcileReport): StatusReport {
  const now = Date.now();
  const cutoff = new Date(now - COMPACTION_AGE_DAYS * 86_400_000).toISOString();

  const pendingConflicts = store.index.pendingConflicts().map((c) => ({
    aId: c.aId,
    bId: c.bId,
    aTitle: store.index.getById(c.aId)?.title ?? "(missing)",
    bTitle: store.index.getById(c.bId)?.title ?? "(missing)",
    detectedAt: c.detectedAt,
  }));

  const today = new Date(now).toISOString().slice(0, 10);
  const reviewDue = store.index.reviewDue(today).map((row) => ({
    id: row.id,
    title: row.title,
    reviewAfter: row.reviewAfter,
  }));

  // Distillation candidates: active episodes that are old and cold. The server
  // only proposes — the caller distills them into a knowledge memory that
  // supersedes the episodes (knowledge first, then the originals sink).
  const compactionCandidates = store.index
    .listByState("active")
    .filter((row) => row.type === "episode" && row.updated !== "" && row.updated < cutoff)
    .map((row) => ({ row, usage: store.index.usageOf(row.id) }))
    .filter(({ usage }) => usage.lastRecalled === "" || usage.lastRecalled < cutoff)
    .slice(0, 10)
    .map(({ row, usage }) => ({
      id: row.id,
      title: row.title,
      type: row.type,
      updated: row.updated,
      recallCount: usage.recallCount,
      hint: "read related episodes, distill into one `knowledge` memory that supersedes them",
    }));

  const unparseable = store.index.listUnparseable();
  const notes: string[] = [
    "coverage is best-effort: absence from these lists is not proof of completeness",
  ];
  if (unparseable.length > 0) {
    notes.push(`${unparseable.length} file(s) could not be indexed — they are invisible to recall`);
  }

  return {
    store: store.storeDir,
    counts: {
      total: store.index.totalCount(),
      byState: store.index.countByColumn("state"),
      byType: store.index.countByColumn("type"),
      byTrust: store.index.countByColumn("trust"),
    },
    pendingConflicts,
    reviewDue,
    compactionCandidates,
    unparseable,
    index: {
      lastReconcile: store.index.getMeta("last_reconcile"),
      lastScan,
      dictionary: {
        present: store.dictionary.entries.length > 0,
        version: store.dictionary.version,
        entries: store.dictionary.entries.length,
      },
    },
    notes,
  };
}
