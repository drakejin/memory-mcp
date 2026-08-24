import {
  MEMORY_STATES,
  MEMORY_TYPES,
  type MemoryFile,
  type MemoryFrontmatter,
  type MemoryState,
  type MemoryType,
  StoreError,
  TRUST_LEVELS,
  type TrustLevel,
} from "./types.ts";

// Strict YAML subset: `key: scalar`, `key: [a, b]`, and block lists
//   key:
//     - a
// Scalars may be double-quoted. Nested maps are rejected. A parse failure makes
// the file "unparseable" — it is excluded from the index and reported by
// memory_status instead of being silently half-read.

const KNOWN_LIST_KEYS = new Set(["entities", "supersedes"]);
const KEY_ORDER = [
  "id",
  "type",
  "state",
  "trust",
  "entities",
  "supersedes",
  "superseded_by",
  "source",
  "created",
  "updated",
  "review_after",
  "deprecated_reason",
];

function unquote(raw: string): string {
  const v = raw.trim();
  if (v.length >= 2 && v.startsWith('"') && v.endsWith('"')) {
    return v.slice(1, -1).replace(/\\"/g, '"');
  }
  return v;
}

function parseInlineList(raw: string): string[] {
  const inner = raw.trim().slice(1, -1).trim();
  if (inner === "") return [];
  return inner
    .split(",")
    .map((part) => unquote(part))
    .filter((part) => part.length > 0);
}

export function parseFrontmatterBlock(lines: string[]): Record<string, string | string[]> {
  const out: Record<string, string | string[]> = {};
  let pendingListKey: string | null = null;
  for (const line of lines) {
    if (line.trim() === "") continue;
    const listItem = /^\s+-\s+(.*)$/.exec(line);
    if (listItem) {
      if (!pendingListKey) {
        throw new StoreError("frontmatter_parse", `list item without a key: ${line.trim()}`);
      }
      (out[pendingListKey] as string[]).push(unquote(listItem[1] ?? ""));
      continue;
    }
    const kv = /^([A-Za-z_][A-Za-z0-9_]*):(.*)$/.exec(line);
    if (!kv) {
      throw new StoreError("frontmatter_parse", `unrecognized frontmatter line: ${line.trim()}`);
    }
    const key = kv[1] as string;
    const rawValue = (kv[2] ?? "").trim();
    pendingListKey = null;
    if (rawValue === "") {
      out[key] = [];
      pendingListKey = key;
    } else if (rawValue.startsWith("[")) {
      if (!rawValue.endsWith("]")) {
        throw new StoreError("frontmatter_parse", `unterminated list for key: ${key}`);
      }
      out[key] = parseInlineList(rawValue);
    } else {
      out[key] = unquote(rawValue);
    }
  }
  return out;
}

function asString(value: string | string[] | undefined, fallback = ""): string {
  if (value === undefined) return fallback;
  if (Array.isArray(value)) {
    throw new StoreError("frontmatter_parse", "expected scalar, got list");
  }
  return value;
}

function asList(value: string | string[] | undefined): string[] {
  if (value === undefined) return [];
  if (Array.isArray(value)) return value;
  return value === "" ? [] : [value];
}

function oneOf<T extends string>(value: string, allowed: readonly T[], field: string): T {
  if ((allowed as readonly string[]).includes(value)) return value as T;
  throw new StoreError(
    "frontmatter_parse",
    `invalid ${field}: "${value}" (allowed: ${allowed.join(", ")})`,
  );
}

export function parseMemoryFile(text: string): MemoryFile {
  if (!text.startsWith("---\n")) {
    throw new StoreError("frontmatter_parse", "missing frontmatter opening delimiter");
  }
  const end = text.indexOf("\n---\n", 4);
  if (end < 0) {
    throw new StoreError("frontmatter_parse", "missing frontmatter closing delimiter");
  }
  const raw = parseFrontmatterBlock(text.slice(4, end + 1).split("\n"));
  const body = text.slice(end + 5).replace(/^\n+/, "");

  const front: MemoryFrontmatter = {
    id: asString(raw.id),
    type: oneOf<MemoryType>(asString(raw.type, "fact"), MEMORY_TYPES, "type"),
    state: oneOf<MemoryState>(asString(raw.state, "active"), MEMORY_STATES, "state"),
    trust: oneOf<TrustLevel>(asString(raw.trust, "agent-inferred"), TRUST_LEVELS, "trust"),
    entities: asList(raw.entities),
    supersedes: asList(raw.supersedes),
    supersededBy: asString(raw.superseded_by),
    source: asString(raw.source),
    created: asString(raw.created),
    updated: asString(raw.updated),
    reviewAfter: asString(raw.review_after),
    deprecatedReason: asString(raw.deprecated_reason),
    extra: {},
  };
  if (front.id === "") {
    throw new StoreError("frontmatter_parse", "missing required frontmatter key: id");
  }
  const consumed = new Set([...KEY_ORDER]);
  for (const [key, value] of Object.entries(raw)) {
    if (!consumed.has(key)) front.extra[key] = value;
  }
  return { front, body };
}

function needsQuote(value: string): boolean {
  return /[:#[\]{}"'\n]|^\s|\s$/.test(value) || value === "";
}

function scalar(value: string): string {
  return needsQuote(value) ? `"${value.replace(/"/g, '\\"')}"` : value;
}

function listLine(values: string[]): string {
  return `[${values.map((v) => scalar(v)).join(", ")}]`;
}

export function serializeMemoryFile(file: MemoryFile): string {
  const f = file.front;
  const lines: string[] = ["---"];
  const emit = (key: string, value: string | string[]) => {
    if (Array.isArray(value)) {
      if (value.length > 0 || KNOWN_LIST_KEYS.has(key)) lines.push(`${key}: ${listLine(value)}`);
    } else if (value !== "") {
      lines.push(`${key}: ${scalar(value)}`);
    }
  };
  emit("id", f.id);
  emit("type", f.type);
  emit("state", f.state);
  emit("trust", f.trust);
  emit("entities", f.entities);
  if (f.supersedes.length > 0) emit("supersedes", f.supersedes);
  emit("superseded_by", f.supersededBy);
  emit("source", f.source);
  emit("created", f.created);
  emit("updated", f.updated);
  emit("review_after", f.reviewAfter);
  emit("deprecated_reason", f.deprecatedReason);
  for (const [key, value] of Object.entries(f.extra)) emit(key, value);
  lines.push("---", "");
  return `${lines.join("\n")}${file.body.endsWith("\n") ? file.body : `${file.body}\n`}`;
}

/** First `# ` heading, else the first non-empty line, trimmed to 120 chars. */
export function deriveTitle(body: string): string {
  for (const line of body.split("\n")) {
    const t = line.trim();
    if (t === "") continue;
    if (t.startsWith("#")) return t.replace(/^#+\s*/, "").slice(0, 120);
    return t.slice(0, 120);
  }
  return "(untitled)";
}

/** First non-heading paragraph, single-line, trimmed to 200 chars. */
export function deriveExcerpt(body: string): string {
  const paragraphs = body.split(/\n\s*\n/);
  for (const p of paragraphs) {
    const t = p
      .split("\n")
      .filter((line) => !line.trim().startsWith("#"))
      .join(" ")
      .replace(/\s+/g, " ")
      .trim();
    if (t !== "") return t.slice(0, 200);
  }
  return "";
}
