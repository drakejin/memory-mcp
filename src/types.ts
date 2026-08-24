export const MEMORY_TYPES = [
  "fact",
  "preference",
  "decision",
  "episode",
  "reference",
  "knowledge",
] as const;
export type MemoryType = (typeof MEMORY_TYPES)[number];

export const MEMORY_STATES = ["active", "archived", "deprecated"] as const;
export type MemoryState = (typeof MEMORY_STATES)[number];

export const TRUST_LEVELS = ["user-stated", "agent-inferred", "imported"] as const;
export type TrustLevel = (typeof TRUST_LEVELS)[number];

// Ranking weights. Deterministic and documented — recall responses expose the
// per-result components so callers can audit why something ranked where it did.
export const TRUST_WEIGHT: Record<TrustLevel, number> = {
  "user-stated": 1.0,
  "agent-inferred": 0.85,
  imported: 0.7,
};

export const STATE_WEIGHT: Record<MemoryState, number> = {
  active: 1.0,
  archived: 0.5,
  deprecated: 0.25,
};

// A conflict pair whose effective-time gap exceeds this window is flagged
// latest_wins_eligible: the caller may confidently supersede the older side.
export const AMBIGUITY_WINDOW_DAYS = 7;

// memory_status surfaces active episodes untouched for this long as
// distillation (compaction) candidates. Nothing is moved automatically.
export const COMPACTION_AGE_DAYS = 30;

export interface MemoryFrontmatter {
  id: string;
  type: MemoryType;
  state: MemoryState;
  trust: TrustLevel;
  entities: string[];
  supersedes: string[];
  supersededBy: string;
  source: string;
  created: string;
  updated: string;
  reviewAfter: string;
  deprecatedReason: string;
  /** Unknown frontmatter keys, preserved verbatim on rewrite. */
  extra: Record<string, string | string[]>;
}

export interface MemoryFile {
  front: MemoryFrontmatter;
  body: string;
}

export interface MemoryRow {
  id: string;
  relPath: string;
  title: string;
  type: MemoryType;
  state: MemoryState;
  trust: TrustLevel;
  source: string;
  created: string;
  updated: string;
  reviewAfter: string;
  supersededBy: string;
  contentSha: string;
  bodySha: string;
  excerpt: string;
  mtimeNs: string;
  size: number;
}

export interface ConflictRow {
  pairHash: string;
  aId: string;
  bId: string;
  status: "pending" | "resolved";
  detectedAt: string;
  resolvedAt: string;
  decision: string;
}

export interface ReconcileReport {
  scanned: number;
  indexed: number;
  removed: number;
  promoted: string[];
  unparseable: number;
  skipped: boolean;
}

export class StoreError extends Error {
  readonly code: string;
  constructor(code: string, message: string) {
    super(message);
    this.code = code;
  }
}
