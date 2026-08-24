import assert from "node:assert/strict";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { detectConflicts, pairHash } from "../src/conflict.ts";
import { MemoryStore, type RememberInput } from "../src/store.ts";

function makeStore(): { store: MemoryStore; dir: string } {
  const dir = mkdtempSync(join(tmpdir(), "memory-mcp-"));
  return { store: new MemoryStore(dir), dir };
}

function input(partial: Partial<RememberInput> & { content: string }): RememberInput {
  return {
    type: "fact",
    trust: "agent-inferred",
    entities: [],
    source: "",
    supersedes: [],
    reviewAfter: "",
    ...partial,
  };
}

test("pairHash is order-independent", () => {
  assert.equal(pairHash("aaa", "bbb"), pairHash("bbb", "aaa"));
});

test("remember surfaces a similar older memory as judgment material", (t) => {
  const { store, dir } = makeStore();
  t.after(() => {
    store.close();
    rmSync(dir, { recursive: true, force: true });
  });
  store.remember(
    input({
      content: "# Deploy target\n\nthe deploy target cluster is alpha-kr",
      entities: ["deploy"],
    }),
  );
  const fresh = store.remember(
    input({
      content: "# Deploy target\n\nthe deploy target cluster is prod-kr",
      entities: ["deploy"],
    }),
  );
  assert.equal(fresh.conflicts.length, 1);
  assert.equal(fresh.conflicts[0]?.title, "Deploy target");
  assert.deepEqual(fresh.conflicts[0]?.sharedEntities, ["deploy"]);
  assert.equal(fresh.conflicts[0]?.latestWinsEligible, false);
  assert.equal(fresh.conflicts[0]?.alreadySurfaced, false);
  assert.equal(store.index.pendingConflicts().length, 1);
});

test("a surfaced pair is idempotent and keep-both silences it", (t) => {
  const { store, dir } = makeStore();
  t.after(() => {
    store.close();
    rmSync(dir, { recursive: true, force: true });
  });
  const a = store.remember(input({ content: "# Same topic\n\nvalue is one", entities: ["topic"] }));
  const b = store.remember(input({ content: "# Same topic\n\nvalue is two", entities: ["topic"] }));
  assert.equal(b.conflicts.length, 1);
  assert.equal(b.conflicts[0]?.alreadySurfaced, false);
  assert.equal(store.index.pendingConflicts().length, 1);

  const again = detectConflicts(store, b.row, ["topic"]);
  assert.equal(again.length, 1);
  assert.equal(again[0]?.alreadySurfaced, true);
  assert.equal(store.index.pendingConflicts().length, 1);

  store.index.resolveConflictsBetween(a.row.id, b.row.id, "keep-both", new Date().toISOString());
  assert.equal(store.index.pendingConflicts().length, 0);

  const after = detectConflicts(store, b.row, ["topic"]);
  assert.equal(after.length, 0, "a resolved pair must not be re-litigated");
});

test("superseding resolves the pending conflict between the pair", (t) => {
  const { store, dir } = makeStore();
  t.after(() => {
    store.close();
    rmSync(dir, { recursive: true, force: true });
  });
  const oldOne = store.remember(
    input({ content: "# Quota\n\nquota is 100 rps", entities: ["quota"] }),
  );
  const newOne = store.remember(
    input({ content: "# Quota\n\nquota is 500 rps", entities: ["quota"] }),
  );
  assert.equal(newOne.conflicts.length, 1);
  assert.equal(store.index.pendingConflicts().length, 1);
  store.applySupersede(newOne.row.id, oldOne.row.id);
  assert.equal(store.index.pendingConflicts().length, 0);
  assert.equal(store.index.getById(oldOne.row.id)?.supersededBy, newOne.row.id);
});

test("superseded and deprecated memories are not conflict candidates", (t) => {
  const { store, dir } = makeStore();
  t.after(() => {
    store.close();
    rmSync(dir, { recursive: true, force: true });
  });
  const v1 = store.remember(input({ content: "# Rule\n\nold rule text", entities: ["rule"] }));
  const v2 = store.remember(
    input({ content: "# Rule\n\nnew rule text", supersedes: [v1.row.id], entities: ["rule"] }),
  );
  const v3 = store.remember(input({ content: "# Rule\n\nnewest rule text", entities: ["rule"] }));
  const ids = v3.conflicts.map((c) => c.id);
  assert.ok(!ids.includes(v1.row.id), "superseded memory must not be re-litigated");
  assert.ok(ids.includes(v2.row.id));
});
