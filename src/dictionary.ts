import { createHash } from "node:crypto";

// Optional controlled vocabulary at <store>/dictionary.md. Lines of the form:
//   - name: one-line description (aka: alias1, alias2)
// normalize() maps aliases (case-insensitive) to the canonical name. Entities
// not in the dictionary are legal — they are surfaced back to the caller as
// candidates so the human can promote them. The dictionary version is its
// content hash: bumping the file invalidates entity normalization the same way
// a file edit invalidates its index row.

export interface DictionaryEntry {
  name: string;
  description: string;
  aliases: string[];
}

export interface Dictionary {
  version: string;
  entries: DictionaryEntry[];
  canonical: Map<string, string>;
}

export const EMPTY_DICTIONARY: Dictionary = {
  version: "",
  entries: [],
  canonical: new Map(),
};

const ENTRY_RE = /^[-*]\s+([^:]+):\s*(.*)$/;
const AKA_RE = /\(aka:\s*([^)]*)\)\s*$/i;

export function parseDictionary(text: string): Dictionary {
  const entries: DictionaryEntry[] = [];
  const canonical = new Map<string, string>();
  for (const line of text.split("\n")) {
    const m = ENTRY_RE.exec(line.trim());
    if (!m) continue;
    const name = (m[1] ?? "").trim();
    let description = (m[2] ?? "").trim();
    if (name === "") continue;
    const aliases: string[] = [];
    const aka = AKA_RE.exec(description);
    if (aka) {
      for (const alias of (aka[1] ?? "").split(",")) {
        const a = alias.trim();
        if (a !== "") aliases.push(a);
      }
      description = description.replace(AKA_RE, "").trim();
    }
    entries.push({ name, description, aliases });
    canonical.set(name.toLowerCase(), name);
    for (const alias of aliases) canonical.set(alias.toLowerCase(), name);
  }
  const version = createHash("sha256").update(text).digest("hex").slice(0, 16);
  return { version, entries, canonical };
}

export interface NormalizedEntities {
  entities: string[];
  /** Inputs not found in the dictionary — candidates for promotion. */
  unknown: string[];
}

export function normalizeEntities(dictionary: Dictionary, inputs: string[]): NormalizedEntities {
  const seen = new Set<string>();
  const entities: string[] = [];
  const unknown: string[] = [];
  for (const input of inputs) {
    const trimmed = input.trim();
    if (trimmed === "") continue;
    const canonical = dictionary.canonical.get(trimmed.toLowerCase());
    const value = canonical ?? trimmed;
    if (!seen.has(value)) {
      seen.add(value);
      entities.push(value);
    }
    if (!canonical && dictionary.entries.length > 0 && !unknown.includes(trimmed)) {
      unknown.push(trimmed);
    }
  }
  return { entities, unknown };
}
