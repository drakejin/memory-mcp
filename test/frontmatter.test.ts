import assert from "node:assert/strict";
import { test } from "node:test";
import {
  deriveExcerpt,
  deriveTitle,
  parseMemoryFile,
  serializeMemoryFile,
} from "../src/frontmatter.ts";
import { StoreError } from "../src/types.ts";

const SAMPLE = `---
id: 01JD2K3A9FZQ4W8XVBH5T6N7RM
type: decision
state: active
trust: user-stated
entities: [ads-genisys, pacing]
source: "session: 2026-08-24"
created: 2026-08-24T14:03:00Z
updated: 2026-08-24T14:03:00Z
custom_key: hello
---
# Pacing decision

We decided to keep the pacing key per ad type.
`;

test("parse extracts known keys and preserves unknown keys", () => {
  const file = parseMemoryFile(SAMPLE);
  assert.equal(file.front.id, "01JD2K3A9FZQ4W8XVBH5T6N7RM");
  assert.equal(file.front.type, "decision");
  assert.equal(file.front.trust, "user-stated");
  assert.deepEqual(file.front.entities, ["ads-genisys", "pacing"]);
  assert.equal(file.front.source, "session: 2026-08-24");
  assert.deepEqual(file.front.extra, { custom_key: "hello" });
  assert.ok(file.body.startsWith("# Pacing decision"));
});

test("serialize -> parse roundtrip is stable", () => {
  const file = parseMemoryFile(SAMPLE);
  const text = serializeMemoryFile(file);
  const reparsed = parseMemoryFile(text);
  assert.deepEqual(reparsed.front, file.front);
  assert.equal(reparsed.body, file.body);
  assert.equal(serializeMemoryFile(reparsed), text);
});

test("block lists parse", () => {
  const text = `---\nid: 01JD2K3A9FZQ4W8XVBH5T6N7RM\nentities:\n  - one\n  - two\n---\nbody\n`;
  const file = parseMemoryFile(text);
  assert.deepEqual(file.front.entities, ["one", "two"]);
});

test("invalid state is a parse error, not silent acceptance", () => {
  const text = `---\nid: 01JD2K3A9FZQ4W8XVBH5T6N7RM\nstate: zombie\n---\nbody\n`;
  assert.throws(
    () => parseMemoryFile(text),
    (err: unknown) => {
      assert.ok(err instanceof StoreError);
      assert.equal(err.code, "frontmatter_parse");
      return true;
    },
  );
});

test("missing id rejected", () => {
  assert.throws(() => parseMemoryFile("---\ntype: fact\n---\nbody\n"));
});

test("title and excerpt derivation", () => {
  assert.equal(deriveTitle("# Hello world\n\nbody"), "Hello world");
  assert.equal(deriveTitle("plain first line\nrest"), "plain first line");
  assert.equal(
    deriveExcerpt("# H\n\nfirst paragraph here\nsecond line\n\nnext"),
    "first paragraph here second line",
  );
});
