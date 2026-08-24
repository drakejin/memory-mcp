import assert from "node:assert/strict";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
import { planQuery, recall } from "../src/search.ts";
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

function baseParams() {
  return {
    query: "",
    entities: [] as string[],
    types: [] as never[],
    includeArchived: false,
    includeDeprecated: false,
    limit: 10,
  };
}

test("query planner routes tokens by script and length", () => {
  const plan = planQuery("보안 설정을 끄는 방법 tracking");
  assert.ok(plan.wordQuery.includes('"보안"*'));
  assert.ok(plan.wordQuery.includes('"tracking"*'));
  assert.ok(plan.triQuery.includes('"설정을"'));
  assert.ok(plan.likeTokens.includes("보안"));
  assert.ok(plan.likeTokens.includes("끄는"));
});

test("korean particle variations are found via prefix and trigram", (t) => {
  const { store, dir } = makeStore();
  t.after(() => {
    store.close();
    rmSync(dir, { recursive: true, force: true });
  });
  store.remember(input({ content: "# 보안 가이드\n\n보안을 끄고 재시작하면 위험하다" }));
  store.remember(input({ content: "# unrelated\n\ncompletely different english text" }));

  const byShortKorean = recall(store, { ...baseParams(), query: "보안" });
  assert.equal(byShortKorean.length, 1);
  assert.equal(byShortKorean[0]?.row.title, "보안 가이드");

  const byParticleVariant = recall(store, { ...baseParams(), query: "보안을 끄는" });
  assert.equal(byParticleVariant.length, 1);

  const byLatin = recall(store, { ...baseParams(), query: "english" });
  assert.equal(byLatin.length, 1);
  assert.equal(byLatin[0]?.row.title, "unrelated");
});

test("user-stated outranks agent-inferred for the same text", (t) => {
  const { store, dir } = makeStore();
  t.after(() => {
    store.close();
    rmSync(dir, { recursive: true, force: true });
  });
  store.remember(
    input({
      content: "# Preference A\n\nprefers tabs for indentation style",
      trust: "agent-inferred",
    }),
  );
  store.remember(
    input({
      content: "# Preference B\n\nprefers spaces for indentation style",
      trust: "user-stated",
    }),
  );
  const hits = recall(store, { ...baseParams(), query: "indentation style" });
  assert.equal(hits.length, 2);
  assert.equal(hits[0]?.row.trust, "user-stated");
});

test("archived memories are hidden by default and sink when included", (t) => {
  const { store, dir } = makeStore();
  t.after(() => {
    store.close();
    rmSync(dir, { recursive: true, force: true });
  });
  const oldOne = store.remember(input({ content: "# Fact v1\n\nthe quota is 100 per day" }));
  store.remember(
    input({ content: "# Fact v2\n\nthe quota is 500 per day", supersedes: [oldOne.row.id] }),
  );

  const defaults = recall(store, { ...baseParams(), query: "quota" });
  assert.equal(defaults.length, 1);
  assert.equal(defaults[0]?.row.title, "Fact v2");

  const widened = recall(store, { ...baseParams(), query: "quota", includeArchived: true });
  assert.equal(widened.length, 2);
  assert.equal(widened[0]?.row.title, "Fact v2");
  assert.equal(widened[1]?.row.state, "archived");
});

test("entity-only recall works without a text query", (t) => {
  const { store, dir } = makeStore();
  t.after(() => {
    store.close();
    rmSync(dir, { recursive: true, force: true });
  });
  store.remember(input({ content: "# Tagged\n\nbody", entities: ["ads-datacat"] }));
  store.remember(input({ content: "# Untagged\n\nbody two" }));
  const hits = recall(store, { ...baseParams(), entities: ["ads-datacat"] });
  assert.equal(hits.length, 1);
  assert.equal(hits[0]?.row.title, "Tagged");
});
