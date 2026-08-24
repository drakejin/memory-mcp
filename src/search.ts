import type { MemoryStore } from "./store.ts";
import {
  type MemoryRow,
  type MemoryState,
  type MemoryType,
  STATE_WEIGHT,
  TRUST_WEIGHT,
} from "./types.ts";

// Hybrid lexical search without embeddings (vectors deferred, same call as the
// AX policy). Two FTS5 indexes plus a LIKE fallback, routed per token:
//   latin/number      -> word FTS prefix query ("tok"*)        — BM25
//   CJK, >= 3 chars   -> trigram phrase ("tok") + word prefix  — substring match
//   CJK, <= 2 chars   -> word prefix + LIKE scan               — below trigram minimum
// Measured on node:sqlite / SQLite 3.50: trigram cannot match tokens shorter
// than 3 chars and phrases must be quoted; the planner absorbs both.

const CJK_RE = /[ᄀ-ᇿ぀-ヿ㄰-㆏一-鿿가-힯]/;

export interface QueryPlan {
  wordQuery: string;
  triQuery: string;
  likeTokens: string[];
  tokens: string[];
}

function ftsPhrase(token: string): string {
  return `"${token.replace(/"/g, '""')}"`;
}

export function planQuery(query: string): QueryPlan {
  const tokens = query
    .split(/\s+/)
    .map((t) => t.replace(/["*():^{}[\]]/g, "").trim())
    .filter((t) => t.length > 0);
  const wordTerms: string[] = [];
  const triTerms: string[] = [];
  const likeTokens: string[] = [];
  for (const token of tokens) {
    wordTerms.push(`${ftsPhrase(token)}*`);
    if (CJK_RE.test(token)) {
      if (token.length >= 3) triTerms.push(ftsPhrase(token));
      else likeTokens.push(token);
    } else if (token.length >= 3) {
      triTerms.push(ftsPhrase(token));
    }
  }
  return {
    wordQuery: wordTerms.join(" OR "),
    triQuery: triTerms.join(" OR "),
    likeTokens,
    tokens,
  };
}

export interface RecallParams {
  query: string;
  entities: string[];
  types: MemoryType[];
  includeArchived: boolean;
  includeDeprecated: boolean;
  limit: number;
}

export interface ScoreComponents {
  text: number;
  trustWeight: number;
  stateWeight: number;
  freshness: number;
  heat: number;
}

export interface RecallHit {
  row: MemoryRow;
  score: number;
  components: ScoreComponents;
  matchedEntities: string[];
}

function daysSince(iso: string, now: number): number {
  if (iso === "") return Number.POSITIVE_INFINITY;
  const t = Date.parse(iso);
  if (Number.isNaN(t)) return Number.POSITIVE_INFINITY;
  return (now - t) / 86_400_000;
}

export function freshnessBoost(updated: string, now: number): number {
  const days = daysSince(updated, now);
  if (days <= 7) return 0.15;
  if (days <= 30) return 0.05;
  return 0;
}

export function heatBoost(recallCount: number): number {
  return Math.min(0.2, recallCount * 0.02);
}

/**
 * Old, unused memories are never deleted by ranking — they sink:
 * score = text * trust_w * state_w * (1 + freshness + heat).
 */
export function recall(store: MemoryStore, params: RecallParams): RecallHit[] {
  const candidateLimit = Math.max(params.limit * 8, 64);
  const textScores = new Map<string, number>();

  // FTS matches score 0.5 + bm25 so a weak-but-real FTS hit never ranks below
  // the LIKE fallback baseline (0.5); bm25 magnitudes collapse toward 0 on tiny
  // corpora, so the baseline also keeps scores legible.
  const ftsScore = (raw: number) => 0.5 + Math.max(0, raw);
  if (params.query.trim() !== "") {
    const plan = planQuery(params.query);
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
    for (const token of plan.likeTokens) {
      for (const id of store.index.searchLike(token, candidateLimit)) {
        if (!textScores.has(id)) textScores.set(id, 0.5);
      }
    }
  }

  const entityIds = store.index.idsByEntities(params.entities);
  if (params.query.trim() === "") {
    for (const id of entityIds) textScores.set(id, 1);
  } else if (params.entities.length > 0) {
    for (const id of textScores.keys()) {
      if (!entityIds.has(id)) textScores.delete(id);
    }
    for (const id of entityIds) {
      if (!textScores.has(id)) textScores.set(id, 0.5);
    }
  }

  const now = Date.now();
  const requestedEntities = new Set(params.entities);
  const hits: RecallHit[] = [];
  for (const [id, text] of textScores) {
    const row = store.index.getById(id);
    if (!row) continue;
    if (row.state === "archived" && !params.includeArchived) continue;
    if (row.state === "deprecated" && !params.includeDeprecated) continue;
    if (params.types.length > 0 && !params.types.includes(row.type)) continue;
    const usage = store.index.usageOf(id);
    const components: ScoreComponents = {
      text,
      trustWeight: TRUST_WEIGHT[row.trust],
      stateWeight: STATE_WEIGHT[row.state as MemoryState],
      freshness: freshnessBoost(row.updated, now),
      heat: heatBoost(usage.recallCount),
    };
    const score =
      components.text *
      components.trustWeight *
      components.stateWeight *
      (1 + components.freshness + components.heat);
    const matchedEntities = store.index.entitiesOf(id).filter((e) => requestedEntities.has(e));
    hits.push({ row, score, components, matchedEntities });
  }

  hits.sort((a, b) => {
    if (b.score !== a.score) return b.score - a.score;
    if (b.row.updated !== a.row.updated) return b.row.updated < a.row.updated ? -1 : 1;
    return a.row.id < b.row.id ? -1 : 1;
  });
  return hits.slice(0, params.limit);
}
