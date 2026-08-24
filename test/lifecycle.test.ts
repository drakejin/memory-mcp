import assert from "node:assert/strict";
import { existsSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { MemoryStore, type RememberInput } from "../src/store.ts";
import { StoreError } from "../src/types.ts";

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

test("supersede archives the loser and links the chain both ways", (t) => {
  const { store, dir } = makeStore();
  t.after(() => {
    store.close();
    rmSync(dir, { recursive: true, force: true });
  });
  const v1 = store.remember(input({ content: "# Car\n\nUser drives a Honda." }));
  const v2 = store.remember(
    input({ content: "# Car\n\nUser switched to a Tesla.", supersedes: [v1.row.id] }),
  );
  assert.deepEqual(v2.supersededIds, [v1.row.id]);

  const loser = store.index.getById(v1.row.id);
  assert.equal(loser?.state, "archived");
  assert.equal(loser?.supersededBy, v2.row.id);

  const chainFromOld = store.chainOf(v1.row.id);
  assert.deepEqual(chainFromOld.supersededBy, [v2.row.id]);
  const chainFromNew = store.chainOf(v2.row.id);
  assert.deepEqual(chainFromNew.supersedes, [v1.row.id]);
});

test("deprecate requires a reason and preserves it", (t) => {
  const { store, dir } = makeStore();
  t.after(() => {
    store.close();
    rmSync(dir, { recursive: true, force: true });
  });
  const m = store.remember(input({ content: "# Wrong\n\nturned out false" }));
  assert.throws(
    () => store.setState(m.row.id, "deprecated"),
    (err: unknown) => err instanceof StoreError && err.code === "reason_required",
  );
  store.setState(m.row.id, "deprecated", "contradicted by prod logs 2026-08-20");
  const { file } = store.readMemoryFileById(m.row.id);
  assert.equal(file.front.state, "deprecated");
  assert.equal(file.front.deprecatedReason, "contradicted by prod logs 2026-08-20");
});

test("purge refuses to delete an active memory", (t) => {
  const { store, dir } = makeStore();
  t.after(() => {
    store.close();
    rmSync(dir, { recursive: true, force: true });
  });
  const m = store.remember(input({ content: "# Live\n\nstill active" }));
  assert.throws(
    () => store.purge(m.row.id),
    (err: unknown) => err instanceof StoreError && err.code === "purge_requires_buffer",
  );
  store.setState(m.row.id, "archived");
  const { removedPath } = store.purge(m.row.id);
  assert.ok(!existsSync(join(dir, removedPath)));
  assert.equal(store.index.totalCount(), 0);
});

test("archived memories can be restored to active", (t) => {
  const { store, dir } = makeStore();
  t.after(() => {
    store.close();
    rmSync(dir, { recursive: true, force: true });
  });
  const m = store.remember(input({ content: "# Restore me\n\nbody" }));
  store.setState(m.row.id, "archived");
  store.setState(m.row.id, "active");
  assert.equal(store.index.getById(m.row.id)?.state, "active");
});

test("edit revises in place without changing state", (t) => {
  const { store, dir } = makeStore();
  t.after(() => {
    store.close();
    rmSync(dir, { recursive: true, force: true });
  });
  const m = store.remember(input({ content: "# Title\n\ntypo herre" }));
  store.setState(m.row.id, "archived");
  const { row } = store.edit(m.row.id, { content: "# Title\n\ntypo here, fixed" });
  assert.equal(row.state, "archived");
  assert.ok(row.excerpt.includes("fixed"));
});
