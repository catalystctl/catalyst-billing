// QR codes, generated in the page.
//
// Enrolment shows an `otpauth://` URI. Typing a 32-character base32 secret into an
// authenticator app is possible and unpleasant, and on a phone it is the difference between
// enrolling and giving up; scanning is what every other panel does. The obvious shortcut is an
// image service (`https://chart.example/qr?...`), and it is not available here: the panel's CSP
// allows images and connections from its own origin only, and sending a live TOTP secret to a
// third party is a credential leak in the shape of a convenience. So the code is generated here,
// from the same string, with no network access at all.
//
// This is a byte-mode QR encoder for versions 1–12: mode 0100, a count field of 8 bits (16 from
// version 10), Reed–Solomon over GF(256) with the 0x11D polynomial, the eight standard masks and
// the standard penalty rules. Versions beyond 12 hold more than 290 bytes at level M, and an
// `otpauth://` URI for a normal address is around 150, so the ceiling is deliberate: a version
// table for all forty symbols is 160 more numbers to get wrong, and the caller renders the
// secret and the URI as text when the data does not fit (see `qrModules` returning null).
//
// The encoder is verified against `qrencode` byte for byte by scripts/qr-verify.mjs, and its
// output is decoded back to the input by web/test/qr.test.mjs. Both matter: a QR that is
// internally consistent but unreadable by a phone is worse than no QR, because the customer
// blames their app.

/** Error-correction levels, strongest last. */
export type QrEcc = "L" | "M" | "Q" | "H";

export interface QrCode {
  /** Modules per side, excluding the quiet zone. */
  size: number;
  /** `modules[row][column]` is true where the symbol is dark. */
  modules: boolean[][];
  version: number;
  mask: number;
}

/**
 * A module grid.
 *
 * Reads go through `dark` and writes through `paint` so that the out-of-range behaviour is
 * stated once: a read outside the symbol is light, and a write outside it is dropped. The
 * placement loops rely on both — the finder-pattern loop deliberately walks one module past each
 * edge to draw separators — and `noUncheckedIndexedAccess` would otherwise make every access a
 * narrowing problem in code that is already the hardest thing here to read.
 */
type Grid = boolean[][];

function blankGrid(size: number): Grid {
  return Array.from({ length: size }, () => new Array<boolean>(size).fill(false));
}

function isDark(grid: Grid, row: number, col: number): boolean {
  return grid[row]?.[col] === true;
}

function paint(grid: Grid, row: number, col: number, value: boolean): void {
  const line = grid[row];
  if (line && col >= 0 && col < line.length) line[col] = value;
}

function copyGrid(grid: Grid): Grid {
  return grid.map((row) => row.slice());
}

/** Total codewords (data + error correction) per version, index 1..12. */
const TOTAL_CODEWORDS = [0, 26, 44, 70, 100, 134, 172, 196, 242, 292, 346, 404, 466];

/** Error-correction codewords per block, index 1..12. */
const EC_PER_BLOCK: Record<QrEcc, number[]> = {
  L: [0, 7, 10, 15, 20, 26, 18, 20, 24, 30, 18, 20, 24],
  M: [0, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26, 30, 22],
  Q: [0, 13, 22, 18, 26, 18, 24, 18, 22, 20, 24, 28, 26],
  H: [0, 17, 28, 22, 16, 22, 28, 26, 26, 24, 28, 24, 28],
};

/** Number of error-correction blocks, index 1..12. */
const EC_BLOCKS: Record<QrEcc, number[]> = {
  L: [0, 1, 1, 1, 1, 1, 2, 2, 2, 2, 4, 4, 4],
  M: [0, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5, 5, 8],
  Q: [0, 1, 1, 2, 2, 4, 4, 6, 6, 8, 8, 8, 10],
  H: [0, 1, 1, 2, 4, 4, 4, 5, 6, 8, 8, 11, 11],
};

/** Alignment-pattern centre coordinates per version, index 1..12. */
const ALIGNMENT: number[][] = [
  [],
  [],
  [6, 18],
  [6, 22],
  [6, 26],
  [6, 30],
  [6, 34],
  [6, 22, 38],
  [6, 24, 42],
  [6, 26, 46],
  [6, 28, 50],
  [6, 30, 54],
  [6, 32, 58],
];

/** Remainder bits after the last codeword, index 1..12. */
const REMAINDER_BITS = [0, 0, 7, 7, 7, 7, 7, 0, 0, 0, 0, 0, 0];

/** The two-bit level field that goes into the format information. */
const ECC_FORMAT_BITS: Record<QrEcc, number> = { L: 1, M: 0, Q: 3, H: 2 };

export const QR_MAX_VERSION = 12;

/** The number of data codewords a version holds at a level. */
function dataCodewords(version: number, ecc: QrEcc): number {
  return totalCodewords(version) - ecPerBlock(version, ecc) * ecBlocks(version, ecc);
}

// The tables are indexed by a version that `qrVersionFor` has already bounded, so the lookups
// cannot miss; the accessors say that once instead of at every use.
function totalCodewords(version: number): number {
  return TOTAL_CODEWORDS[version] ?? 0;
}

function ecPerBlock(version: number, ecc: QrEcc): number {
  return EC_PER_BLOCK[ecc][version] ?? 0;
}

function ecBlocks(version: number, ecc: QrEcc): number {
  return EC_BLOCKS[ecc][version] ?? 0;
}

/** Byte-mode capacity in bytes, after the mode indicator and count field. */
export function qrByteCapacity(version: number, ecc: QrEcc): number {
  const countBits = version <= 9 ? 8 : 16;
  return Math.floor((dataCodewords(version, ecc) * 8 - 4 - countBits) / 8);
}

/** The smallest version that fits `length` bytes, or null when none does. */
export function qrVersionFor(length: number, ecc: QrEcc = "M"): number | null {
  for (let version = 1; version <= QR_MAX_VERSION; version++) {
    if (qrByteCapacity(version, ecc) >= length) return version;
  }
  return null;
}

/**
 * Encodes `text` as a QR symbol, or returns null when it does not fit.
 *
 * Text is encoded as UTF-8 bytes. Null rather than an exception because "this string is too long
 * for the version table" is a case the caller has an answer for — show the secret and the URI as
 * text — and a thrown error inside a dialog would take the whole enrolment screen with it.
 */
export function qrModules(text: string, ecc: QrEcc = "M"): QrCode | null {
  const candidates = qrCandidates(text, ecc);
  if (!candidates) return null;
  // The mask whose penalty is lowest wins; the format bits are part of the symbol, so they are
  // re-written per candidate and the scores are comparable.
  let best = candidates[0]!;
  let bestPenalty = Infinity;
  for (const candidate of candidates) {
    const penalty = penaltyScore(candidate.modules, candidate.size);
    if (penalty < bestPenalty) {
      bestPenalty = penalty;
      best = candidate;
    }
  }
  return best;
}

/**
 * All eight masked variants of one symbol, in mask order.
 *
 * Exported for verification rather than for the UI: another encoder may legitimately choose a
 * different mask, so proving this one correct means showing that its output is *one of* the
 * candidates, not that it happens to agree on the penalty score. scripts/qr-verify.mjs uses it
 * to compare against `qrencode` byte for byte.
 */
export function qrCandidates(text: string, ecc: QrEcc = "M"): QrCode[] | null {
  const bytes = new TextEncoder().encode(text);
  const version = qrVersionFor(bytes.length, ecc);
  if (version === null) return null;

  const size = version * 4 + 17;
  const modules = blankGrid(size);
  // Which modules are function patterns (finder, timing, alignment, format, version). They are
  // never masked and never carry data, so they are tracked separately from the module colours.
  const reserved = blankGrid(size);

  placeFunctionPatterns(modules, reserved, version, size);

  const codewords = buildCodewords(bytes, version, ecc);
  placeCodewords(modules, reserved, codewords, size);

  const out: QrCode[] = [];
  for (let mask = 0; mask < 8; mask++) {
    const candidate = copyGrid(modules);
    applyMask(candidate, reserved, mask, size);
    placeFormatInfo(candidate, ecc, mask, size);
    out.push({ size, modules: candidate, version, mask });
  }
  return out;
}

// ------------------------------------------------------------- function patterns --

function placeFunctionPatterns(modules: boolean[][], reserved: boolean[][], version: number, size: number): void {
  const set = (row: number, col: number, value: boolean) => {
    if (row < 0 || col < 0 || row >= size || col >= size) return;
    paint(modules, row, col, value);
    paint(reserved, row, col, true);
  };

  // Timing patterns: the alternating row and column at index 6.
  for (let i = 0; i < size; i++) {
    set(6, i, i % 2 === 0);
    set(i, 6, i % 2 === 0);
  }

  // Finder patterns and their separators, at three corners.
  for (const [row, col] of [
    [0, 0],
    [0, size - 7],
    [size - 7, 0],
  ] as const) {
    for (let dr = -1; dr <= 7; dr++) {
      for (let dc = -1; dc <= 7; dc++) {
        const r = row + dr;
        const c = col + dc;
        if (r < 0 || c < 0 || r >= size || c >= size) continue;
        const inRing = dr >= 0 && dr <= 6 && dc >= 0 && dc <= 6;
        const dark = inRing && (dr === 0 || dr === 6 || dc === 0 || dc === 6 || (dr >= 2 && dr <= 4 && dc >= 2 && dc <= 4));
        set(r, c, dark);
      }
    }
  }

  // Alignment patterns, skipping the three that would land on a finder pattern.
  const centres = ALIGNMENT[version] ?? [];
  for (const row of centres) {
    for (const col of centres) {
      const onFinder =
        (row === 6 && col === 6) || (row === 6 && col === size - 7) || (row === size - 7 && col === 6);
      if (onFinder) continue;
      for (let dr = -2; dr <= 2; dr++) {
        for (let dc = -2; dc <= 2; dc++) {
          const ring = Math.max(Math.abs(dr), Math.abs(dc));
          set(row + dr, col + dc, ring !== 1);
        }
      }
    }
  }

  // Reserve the format-information areas so data placement skips them. The values are written
  // later, once the mask is chosen. Index 6 is skipped: those two cells belong to the timing
  // pattern and are not format bits, and overwriting them here is what made the symbol
  // unreadable by a real scanner.
  for (let i = 0; i < 9; i++) {
    if (i === 6) continue;
    set(8, i, false);
    set(i, 8, false);
  }
  for (let i = 0; i < 8; i++) {
    set(8, size - 1 - i, false);
    set(size - 1 - i, 8, false);
  }
  // The dark module is always dark and always at this position.
  set(size - 8, 8, true);

  // Version information, two blocks of 18 bits, from version 7 upwards.
  if (version >= 7) {
    let rem = version;
    for (let i = 0; i < 12; i++) rem = (rem << 1) ^ ((rem >>> 11) * 0x1f25);
    const bits = (version << 12) | rem;
    for (let i = 0; i < 18; i++) {
      const dark = ((bits >>> i) & 1) === 1;
      const a = size - 11 + (i % 3);
      const b = Math.floor(i / 3);
      set(b, a, dark);
      set(a, b, dark);
    }
  }
}

function placeFormatInfo(modules: boolean[][], ecc: QrEcc, mask: number, size: number): void {
  const data = (ECC_FORMAT_BITS[ecc] << 3) | mask;
  let rem = data;
  for (let i = 0; i < 10; i++) rem = (rem << 1) ^ ((rem >>> 9) * 0x537);
  const bits = ((data << 10) | rem) ^ 0x5412;

  const bit = (i: number) => ((bits >>> i) & 1) === 1;

  // Around the top-left finder: bits 0-5 run down column 8, bit 6 sits beside the timing
  // pattern, and bits 9-14 run left along row 8.
  for (let i = 0; i <= 5; i++) paint(modules, i, 8, bit(i));
  paint(modules, 7, 8, bit(6));
  paint(modules, 8, 8, bit(7));
  paint(modules, 8, 7, bit(8));
  for (let i = 9; i < 15; i++) paint(modules, 8, 14 - i, bit(i));

  // The second copy: bits 0-7 along row 8 to the right of the top-right finder, bits 8-14 down
  // column 8 below the bottom-left finder.
  for (let i = 0; i < 8; i++) paint(modules, 8, size - 1 - i, bit(i));
  for (let i = 8; i < 15; i++) paint(modules, size - 15 + i, 8, bit(i));
  paint(modules, size - 8, 8, true);
}

// ------------------------------------------------------------------- codewords --

function buildCodewords(bytes: Uint8Array, version: number, ecc: QrEcc): number[] {
  const bits: number[] = [];
  const push = (value: number, width: number) => {
    for (let i = width - 1; i >= 0; i--) bits.push((value >>> i) & 1);
  };

  push(0b0100, 4); // byte mode
  push(bytes.length, version <= 9 ? 8 : 16);
  for (const byte of bytes) push(byte, 8);

  const capacityBits = dataCodewords(version, ecc) * 8;
  // Terminator: up to four zero bits, then pad to a byte boundary.
  push(0, Math.min(4, capacityBits - bits.length));
  while (bits.length % 8 !== 0) bits.push(0);

  const data: number[] = [];
  for (let i = 0; i < bits.length; i += 8) {
    let value = 0;
    for (let j = 0; j < 8; j++) value = (value << 1) | (bits[i + j] ?? 0);
    data.push(value);
  }
  // Pad codewords alternate 0xEC / 0x11. They are not arbitrary: the specification names them,
  // and a decoder does not care, but a byte-for-byte comparison against another encoder does.
  for (let i = 0; data.length < capacityBits / 8; i++) data.push(i % 2 === 0 ? 0xec : 0x11);

  return interleave(data, version, ecc);
}

/** Splits data into blocks, adds Reed–Solomon codewords to each, and interleaves. */
function interleave(data: number[], version: number, ecc: QrEcc): number[] {
  const blocks = ecBlocks(version, ecc);
  const ecPer = ecPerBlock(version, ecc);
  const total = totalCodewords(version);

  // Block lengths are not equal for every version: the short blocks carry one codeword fewer.
  const shortBlockLen = Math.floor(total / blocks);
  const shortBlocks = blocks - (total % blocks);
  const dataPerShort = shortBlockLen - ecPer;

  const dataBlocks: number[][] = [];
  const ecBlocksList: number[][] = [];
  let offset = 0;
  for (let i = 0; i < blocks; i++) {
    const length = dataPerShort + (i < shortBlocks ? 0 : 1);
    const block = data.slice(offset, offset + length);
    offset += length;
    dataBlocks.push(block);
    ecBlocksList.push(reedSolomon(block, ecPer));
  }

  const out: number[] = [];
  const longest = dataPerShort + 1;
  for (let i = 0; i < longest; i++) {
    for (const block of dataBlocks) {
      if (i < block.length) out.push(block[i] ?? 0);
    }
  }
  for (let i = 0; i < ecPer; i++) {
    for (const block of ecBlocksList) out.push(block[i] ?? 0);
  }
  return out;
}

/** GF(256) log/antilog tables for the 0x11D polynomial, built once. */
const GF_EXP: number[] = new Array(512);
const GF_LOG: number[] = new Array(256).fill(0);
{
  let x = 1;
  for (let i = 0; i < 255; i++) {
    GF_EXP[i] = x;
    GF_LOG[x] = i;
    x <<= 1;
    if (x & 0x100) x ^= 0x11d;
  }
  for (let i = 255; i < 512; i++) GF_EXP[i] = GF_EXP[i - 255] ?? 0;
}

function gfMul(a: number, b: number): number {
  if (a === 0 || b === 0) return 0;
  return GF_EXP[(GF_LOG[a] ?? 0) + (GF_LOG[b] ?? 0)] ?? 0;
}

/** The generator polynomial for `degree` error-correction codewords. */
function rsGenerator(degree: number): number[] {
  let poly = [1];
  for (let i = 0; i < degree; i++) {
    // Multiply by (x - α^i).
    const next = new Array<number>(poly.length + 1).fill(0);
    for (let j = 0; j < poly.length; j++) {
      const coefficient = poly[j] ?? 0;
      next[j] = (next[j] ?? 0) ^ coefficient;
      next[j + 1] = (next[j + 1] ?? 0) ^ gfMul(coefficient, GF_EXP[i] ?? 0);
    }
    poly = next;
  }
  return poly;
}

/** Remainder of the data polynomial divided by the generator, i.e. the ECC codewords. */
function reedSolomon(data: number[], ecCount: number): number[] {
  const generator = rsGenerator(ecCount);
  const remainder = new Array<number>(ecCount).fill(0);
  for (const byte of data) {
    const factor = byte ^ (remainder[0] ?? 0);
    remainder.shift();
    remainder.push(0);
    for (let i = 0; i < ecCount; i++) {
      remainder[i] = (remainder[i] ?? 0) ^ gfMul(generator[i + 1] ?? 0, factor);
    }
  }
  return remainder;
}

// -------------------------------------------------------------------- placement --

function placeCodewords(modules: boolean[][], reserved: boolean[][], codewords: number[], size: number): void {
  let bitIndex = 0;
  const totalBits = codewords.length * 8;

  // Two-module-wide columns, right to left, alternating upwards and downwards. The vertical
  // timing column (6) is skipped by shifting the left column one to the left past it.
  for (let right = size - 1; right >= 1; right -= 2) {
    if (right === 6) right = 5;
    for (let vert = 0; vert < size; vert++) {
      for (let j = 0; j < 2; j++) {
        const col = right - j;
        const upward = ((right + 1) & 2) === 0;
        const row = upward ? size - 1 - vert : vert;
        if (isDark(reserved, row, col)) continue;
        if (bitIndex < totalBits) {
          const word = codewords[bitIndex >>> 3] ?? 0;
          paint(modules, row, col, ((word >>> (7 - (bitIndex & 7))) & 1) === 1);
          bitIndex++;
        } else {
          // Remainder bits stay light until the mask is applied.
          paint(modules, row, col, false);
        }
      }
    }
  }
}

function maskBit(mask: number, row: number, col: number): boolean {
  switch (mask) {
    case 0:
      return (row + col) % 2 === 0;
    case 1:
      return row % 2 === 0;
    case 2:
      return col % 3 === 0;
    case 3:
      return (row + col) % 3 === 0;
    case 4:
      return (Math.floor(row / 2) + Math.floor(col / 3)) % 2 === 0;
    case 5:
      return ((row * col) % 2) + ((row * col) % 3) === 0;
    case 6:
      return (((row * col) % 2) + ((row * col) % 3)) % 2 === 0;
    case 7:
      return (((row + col) % 2) + ((row * col) % 3)) % 2 === 0;
    default:
      return false;
  }
}

function applyMask(modules: boolean[][], reserved: boolean[][], mask: number, size: number): void {
  for (let row = 0; row < size; row++) {
    for (let col = 0; col < size; col++) {
      if (isDark(reserved, row, col)) continue;
      if (maskBit(mask, row, col)) paint(modules, row, col, !isDark(modules, row, col));
    }
  }
}

// ---------------------------------------------------------------------- penalty --

/** The four penalty rules from the specification, used to choose the mask. */
function penaltyScore(modules: boolean[][], size: number): number {
  let score = 0;

  // Rule 1: runs of five or more same-coloured modules in a row or column.
  for (let i = 0; i < size; i++) {
    for (const horizontal of [true, false]) {
      let run = 1;
      for (let j = 1; j < size; j++) {
        const prev = horizontal ? isDark(modules, i, j - 1) : isDark(modules, j - 1, i);
        const cur = horizontal ? isDark(modules, i, j) : isDark(modules, j, i);
        if (cur === prev) {
          run++;
        } else {
          if (run >= 5) score += 3 + (run - 5);
          run = 1;
        }
      }
      if (run >= 5) score += 3 + (run - 5);
    }
  }

  // Rule 2: every 2×2 block of one colour.
  for (let row = 0; row < size - 1; row++) {
    for (let col = 0; col < size - 1; col++) {
      const a = isDark(modules, row, col);
      if (
        a === isDark(modules, row, col + 1) &&
        a === isDark(modules, row + 1, col) &&
        a === isDark(modules, row + 1, col + 1)
      ) {
        score += 3;
      }
    }
  }

  // Rule 3: the finder-like pattern 1:1:3:1:1 with four light modules on either side.
  const pattern = [true, false, true, true, true, false, true];
  const matches = (get: (i: number) => boolean, start: number) => {
    for (let k = 0; k < 7; k++) if (get(start + k) !== (pattern[k] ?? false)) return false;
    const before = [start - 4, start - 3, start - 2, start - 1].every((i) => i < 0 || get(i) === false);
    const after = [start + 7, start + 8, start + 9, start + 10].every((i) => i >= size || get(i) === false);
    return before || after;
  };
  for (let i = 0; i < size; i++) {
    for (let j = 0; j + 7 <= size; j++) {
      if (matches((k) => isDark(modules, i, k), j)) score += 40;
      if (matches((k) => isDark(modules, k, i), j)) score += 40;
    }
  }

  // Rule 4: deviation of the dark-module proportion from 50%.
  let dark = 0;
  for (const row of modules) for (const module of row) if (module) dark++;
  const percent = (dark * 100) / (size * size);
  score += Math.floor(Math.abs(percent - 50) / 5) * 10;

  return score;
}

/** Renders a symbol as an SVG path, for the DOM. Not used by the tests. */
export function qrSvgPath(code: QrCode): string {
  const parts: string[] = [];
  for (let row = 0; row < code.size; row++) {
    let col = 0;
    while (col < code.size) {
      if (!isDark(code.modules, row, col)) {
        col++;
        continue;
      }
      const start = col;
      while (col < code.size && isDark(code.modules, row, col)) col++;
      parts.push(`M${start} ${row}h${col - start}v1h-${col - start}z`);
    }
  }
  return parts.join("");
}
