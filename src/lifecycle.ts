import { type MemoryState, StoreError } from "./types.ts";

// The state machine, after MemOS (archived buffer before any deletion) and the
// AX lifecycle policy (deprecated = judged wrong, preserved with its reason).
// purge is not a state: it is file deletion, guarded by an explicit confirm.
const TRANSITIONS: Record<string, MemoryState[]> = {
  active: ["archived", "deprecated"],
  archived: ["active", "deprecated"],
  deprecated: ["active"],
};

export function assertTransition(from: MemoryState, to: MemoryState): void {
  if (from === to) return;
  const allowed = TRANSITIONS[from] ?? [];
  if (!allowed.includes(to)) {
    throw new StoreError("invalid_transition", `cannot transition ${from} -> ${to}`);
  }
}

export function canPurge(state: MemoryState): boolean {
  return state === "archived" || state === "deprecated";
}
