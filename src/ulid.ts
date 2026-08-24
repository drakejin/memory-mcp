import { randomBytes } from "node:crypto";

// Crockford base32, monotonic within process. Sortable ids keep memory files
// listable in creation order without reading frontmatter.
const ALPHABET = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";

let lastTime = 0;
let lastRandom: number[] = [];

function encodeTime(time: number): string {
  let out = "";
  let t = time;
  for (let i = 0; i < 10; i++) {
    out = (ALPHABET[t % 32] ?? "0") + out;
    t = Math.floor(t / 32);
  }
  return out;
}

function randomPart(): number[] {
  const bytes = randomBytes(16);
  const digits: number[] = [];
  for (let i = 0; i < 16; i++) {
    digits.push((bytes[i] as number) % 32);
  }
  return digits;
}

function incrementRandom(digits: number[]): number[] {
  const next = [...digits];
  for (let i = next.length - 1; i >= 0; i--) {
    const v = (next[i] as number) + 1;
    if (v < 32) {
      next[i] = v;
      return next;
    }
    next[i] = 0;
  }
  return next; // overflow wraps; practically unreachable
}

export function ulid(now: number = Date.now()): string {
  if (now === lastTime) {
    lastRandom = incrementRandom(lastRandom);
  } else {
    lastTime = now;
    lastRandom = randomPart();
  }
  return encodeTime(now) + lastRandom.map((d) => ALPHABET[d]).join("");
}

export function isUlid(value: string): boolean {
  return /^[0-9A-HJKMNP-TV-Z]{26}$/.test(value);
}
