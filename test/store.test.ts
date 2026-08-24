import assert from "node:assert/strict";
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { test } from "node:test";
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

test("remember writes a parseable file and indexes it", (t) => {
  const { store, dir } = makeStore();
  t.after(() => {
    store.close();
    rmSync(dir, { recursive: true, force: true });
  });
  const result = store.remember(input({ content: "# BA nil rule\n\nInvalid BA ids return nil." }));
  assert.equal(result.row.state, "active");
  assert.equal(result.row.title, "BA nil rule");
  const onDisk = readFileSync(join(dir, result.row.relPath), "utf8");
  assert.ok(onDisk.includes("id: " + result.row.id));
  assert.equal(store.index.totalCount(), 1);
});

test("identical content is idempotent", (t) => {
  const { store, dir } = makeStore();
  t.after(() => {
    store.close();
    rmSync(dir, { recursive: true, force: true });
  });
  const a = store.remember(input({ content: "# Same\n\nsame body" }));
  const b = store.remember(input({ content: "# Same\n\nsame body" }));
  assert.equal(b.duplicateOf, a.row.id);
  assert.equal(store.index.totalCount(), 1);
});

test("reconcile picks up hand-written files and drops deleted ones", (t) => {
  const { store, dir } = makeStore();
  t.after(() => {
    store.close();
    rmSync(dir, { recursive: true, force: true });
  });
  const path = join(dir, "memories", "01JD2K3A9FZQ4W8XVBH5T6N7RM.md");
  writeFileSync(
    path,
    "---\nid: 01JD2K3A9FZQ4W8XVBH5T6N7RM\ntype: fact\nstate: active\ntrust: user-stated\nentities: []\ncreated: 2026-08-01T00:00:00Z\nupdated: 2026-08-01T00:00:00Z\n---\n# Hand written\n\ncontent\n",
  );
  const report = store.reconcile(true);
  assert.equal(report.indexed, 1);
  assert.equal(store.index.getById("01JD2K3A9FZQ4W8XVBH5T6N7RM")?.title, "Hand written");

  rmSync(path);
  const report2 = store.reconcile(true);
  assert.equal(report2.removed, 1);
  assert.equal(store.index.totalCount(), 0);
});

test("unparseable files are reported, not silently dropped", (t) => {
  const { store, dir } = makeStore();
  t.after(() => {
    store.close();
    rmSync(dir, { recursive: true, force: true });
  });
  writeFileSync(join(dir, "memories", "broken.md"), "no frontmatter here");
  const report = store.reconcile(true);
  assert.equal(report.unparseable, 1);
  assert.equal(store.index.listUnparseable()[0]?.relPath, join("memories", "broken.md"));
});

test("hand-editing an archived memory's body promotes it back to active", (t) => {
  const { store, dir } = makeStore();
  t.after(() => {
    store.close();
    rmSync(dir, { recursive: true, force: true });
  });
  const made = store.remember(input({ content: "# Old truth\n\noriginal body" }));
  store.setState(made.row.id, "archived");
  const abs = join(dir, made.row.relPath);
  const text = readFileSync(abs, "utf8");
  writeFileSync(abs, text.replace("original body", "revised by hand"));
  const report = store.reconcile(true);
  assert.deepEqual(report.promoted, [made.row.id]);
  assert.equal(store.index.getById(made.row.id)?.state, "active");
  assert.ok(readFileSync(abs, "utf8").includes("state: active"));
});

test("system frontmatter-only writes do not promote archived memories", (t) => {
  const { store, dir } = makeStore();
  t.after(() => {
    store.close();
    rmSync(dir, { recursive: true, force: true });
  });
  const loser = store.remember(input({ content: "# Loser\n\nold fact body" }));
  const winner = store.remember(input({ content: "# Winner\n\nnew fact body" }));
  store.applySupersede(winner.row.id, loser.row.id);
  assert.equal(store.index.getById(loser.row.id)?.state, "archived");
  const report = store.reconcile(true);
  assert.deepEqual(report.promoted, []);
  assert.equal(store.index.getById(loser.row.id)?.state, "archived");
});

test("rebuildDerived reconstructs everything from files", (t) => {
  const { store, dir } = makeStore();
  t.after(() => {
    store.close();
    rmSync(dir, { recursive: true, force: true });
  });
  const a = store.remember(input({ content: "# A\n\nalpha", entities: ["x"] }));
  store.remember(input({ content: "# B\n\nbeta" }));
  const report = store.rebuildDerived();
  assert.equal(report.indexed, 2);
  assert.equal(store.index.totalCount(), 2);
  assert.deepEqual(store.index.entitiesOf(a.row.id), ["x"]);
});

test("dictionary normalizes entities and reports unknown ones", (t) => {
  const { store, dir } = makeStore();
  t.after(() => {
    store.close();
    rmSync(dir, { recursive: true, force: true });
  });
  writeFileSync(
    join(dir, "dictionary.md"),
    "- ads-genisys: serving backend (aka: genisys, 제니시스)\n",
  );
  store.reconcile(true);
  const result = store.remember(
    input({ content: "# With entities\n\nbody", entities: ["제니시스", "mystery-thing"] }),
  );
  assert.deepEqual(store.index.entitiesOf(result.row.id), ["ads-genisys", "mystery-thing"]);
  assert.deepEqual(result.unknownEntities, ["mystery-thing"]);
});
