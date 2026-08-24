import { createHash } from "node:crypto";
import { planQuery } from "./search.ts";
import type { MemoryStore } from "./store.ts";
import { AMBIGUITY_WINDOW_DAYS, type MemoryRow } from "./types.ts";

// The judgment gate, folded for MCP: the server has no LLM — the caller is
// one. So detection produces *judgment material* (neighbors, overlap evidence,
// a latest-wins eligibility signal) and never overwrites anything by itself.
// pair_hash over the two body hashes makes each content pair surface once
// (AX loop guard: pair-sha idempotency).

export interface ConflictCandidate {
  id: string;
  title: string;
  excerpt: string;
  state: string;
  trust: string;
  updated: string;
  ageDays: number;
  sharedEntities: string[];
  textScore: number;
  latestWinsEligible: boolean;
  alreadySurfaced: boolean;
}

export function pairHash(bodyShaA: string, bodyShaB: string): string {
  const [x, y] = [bodyShaA, bodyShaB].sort();
  return createHash("sha256").update(`${x}:${y}`).digest("hex");
}

const NEIGHBOR_LIMIT = 3;
// FTS hits arrive as 0.5 + bm25; entity-shared neighbors are injected at 0.6 so
// they always clear the gate, while bare FTS matches need real term overlap.
const MIN_TEXT_SCORE = 0.55;
const ENTITY_NEIGHBOR_SCORE = 0.6;

export function detectConflicts(
  store: MemoryStore,
  subject: MemoryRow,
  subjectEntities: string[],
): ConflictCandidate[] {
  const probe = `${subject.title} ${subject.excerpt}`.trim();
  const plan = planQuery(probe);
  const textScores = new Map<string, number>();
  const candidateLimit = 32;
  const ftsScore = (raw: number) => 0.5 + Math.max(0, raw);
  if (plan.wordQuery !== "") {
    for (const [id, score] of store.index.searchWord(plan.wordQuery, candidateLimit)) {
      textScores.set(id, Math.max(textScores.get(id) ?? 0, ftsScore(score)));
    }
  }
  if (plan.triQuery !== "") {
    for (const [id, score] of store.index.searchTrigram(plan.triQuery, candidateLimit)) {
      textScores.set(id, Math.max(textScores.get(id) ?? 0, ftsScore(score)));
    }
  }
  for (const id of store.index.idsByEntities(subjectEntities)) {
    textScores.set(id, Math.max(textScores.get(id) ?? 0, ENTITY_NEIGHBOR_SCORE));
  }

  const subjectTime = Date.parse(subject.updated || subject.created);
  const entitySet = new Set(subjectEntities);
  const out: ConflictCandidate[] = [];
  for (const [id, textScore] of textScores) {
    if (id === subject.id) continue;
    if (textScore < MIN_TEXT_SCORE) continue;
    const row = store.index.getById(id);
    if (!row) continue;
    if (row.state === "deprecated") continue;
    if (row.supersededBy !== "") continue;
    const sharedEntities = store.index.entitiesOf(id).filter((e) => entitySet.has(e));
    const rowTime = Date.parse(row.updated || row.created);
    const gapDays =
      Number.isNaN(subjectTime) || Number.isNaN(rowTime)
        ? 0
        : Math.abs(subjectTime - rowTime) / 86_400_000;
    const hash = pairHash(subject.bodySha, row.bodySha);
    const existing = store.index.getConflict(hash);
    if (existing?.status === "resolved") continue;
    out.push({
      id,
      title: row.title,
      excerpt: row.excerpt,
      state: row.state,
      trust: row.trust,
      updated: row.updated,
      ageDays: Math.round(gapDays * 10) / 10,
      sharedEntities,
      textScore: Math.round(textScore * 100) / 100,
      latestWinsEligible: gapDays > AMBIGUITY_WINDOW_DAYS,
      alreadySurfaced: existing !== undefined,
    });
  }

  out.sort((a, b) => b.textScore - a.textScore);
  const top = out.slice(0, NEIGHBOR_LIMIT);
  const when = new Date().toISOString();
  for (const candidate of top) {
    const row = store.index.getById(candidate.id);
    if (!row) continue;
    store.index.upsertConflict({
      pairHash: pairHash(subject.bodySha, row.bodySha),
      aId: subject.id,
      bId: candidate.id,
      status: "pending",
      detectedAt: when,
      resolvedAt: "",
      decision: "",
    });
  }
  return top;
}
