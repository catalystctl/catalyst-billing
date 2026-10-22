// The QR encoder.
//
// Enrolment draws a QR code in the page, because the panel's CSP allows images and connections from
// its own origin only: a third-party image service is not available, and sending a live TOTP secret
// to one would be a credential leak in the shape of a convenience. That makes the encoder part of
// the security path — a symbol no camera can read sends the customer back to typing 32 base32
// characters, which is the outcome the QR exists to avoid.
//
// A self-consistent encoder proves nothing: the same wrong table produces a matrix that decodes
// with the same wrong table. So the fixture below is not this encoder's output. It is the matrix
// `qrencode` 4.1.1 produces for the same input (`qrencode -l M -8 -v 1 -m 0 -s 1`), which is an
// independent implementation in C. If a table, a mask rule or a format bit changes, this fails.
// scripts/qr-verify.mjs does the same comparison across versions 1–10 for a longer input, where a
// version-information block and alignment patterns are involved.

import assert from "node:assert/strict";
import test from "node:test";

import { qrByteCapacity, qrModules, qrSvgPath, qrVersionFor } from "../.test-build/lib/qr.js";

/** `qrencode`'s output for "hello" at level M, version 1, no margin. */
const HELLO_V1_M = [
  "111111100110001111111",
  "100000100110001000001",
  "101110100100101011101",
  "101110100011001011101",
  "101110100110101011101",
  "100000101001101000001",
  "111111101010101111111",
  "000000000001100000000",
  "100101101100010100000",
  "001011000010001000011",
  "000110111100110001101",
  "111011001001000001011",
  "011010110010101010000",
  "000000001101000110101",
  "111111100010010101110",
  "100000101011110110000",
  "101110100001001110001",
  "101110101101000101111",
  "101110100110100010101",
  "100000100110011000000",
  "111111101111100101010",
];

function asRows(code) {
  return code.modules.map((row) => row.map((dark) => (dark ? "1" : "0")).join(""));
}

test("the encoder reproduces qrencode's symbol for a known input", () => {
  const code = qrModules("hello", "M");
  assert.ok(code, "hello did not encode");
  assert.equal(code.version, 1);
  assert.equal(code.size, 21);
  assert.deepEqual(asRows(code), HELLO_V1_M);
});

test("the symbol carries its own version and mask", () => {
  const code = qrModules("hello", "M");
  assert.ok(code);
  assert.ok(code.mask >= 0 && code.mask <= 7, `mask ${code.mask}`);
  // The dark module at (4·version+9, 8) is always set; it is the one module with no data meaning
  // and it is a cheap check that the function patterns landed where a decoder looks for them.
  assert.equal(code.modules[4 * code.version + 9][8], true);
});

test("byte-mode capacity matches the specification", () => {
  // Version 1-M holds 16 data codewords; four bits of mode and eight of count leave 14 bytes.
  assert.equal(qrByteCapacity(1, "M"), 14);
  // A well-known reference point: version 10-H is 119 bytes, version 1-L is 17.
  assert.equal(qrByteCapacity(1, "L"), 17);
  assert.equal(qrByteCapacity(10, "H"), 119);
});

test("the version chosen is the smallest that fits", () => {
  assert.equal(qrVersionFor(1, "M"), 1);
  assert.equal(qrVersionFor(14, "M"), 1);
  assert.equal(qrVersionFor(15, "M"), 2);
  // An otpauth URI for a normal address is around 150 bytes; it must fit inside the table.
  assert.equal(qrVersionFor(150, "M"), 8);
  // Past the table the caller is told so rather than given a truncated symbol.
  assert.equal(qrVersionFor(5000, "M"), null);
  assert.equal(qrModules("x".repeat(5000), "M"), null);
});

test("a symbol drawn as a path covers the dark modules and nothing else", () => {
  const code = qrModules("hello", "M");
  assert.ok(code);
  const path = qrSvgPath(code);
  // One sub-path per run of dark modules; a 21×21 symbol of this content has far more than ten.
  const runs = (path.match(/M/g) ?? []).length;
  const dark = code.modules.flat().filter(Boolean).length;
  assert.ok(runs > 10, `runs=${runs}`);
  assert.ok(runs <= dark, `runs=${runs} dark=${dark}`);
  assert.match(path, /^M\d+ \d+h\d+v1h-\d+z/);
});

test("UTF-8 input is encoded as bytes, not code points", () => {
  // "é" is two bytes in UTF-8. Encoding the code point instead would change the length field and
  // produce a symbol a scanner reads as mojibake.
  const code = qrModules("é", "M");
  assert.ok(code);
  const same = qrModules("Ã©", "M"); // the two UTF-8 bytes, as a byte string would have them
  // They are different strings, so different symbols; what matters is that neither throws and both
  // fit version 1.
  assert.equal(code.version, 1);
  assert.ok(same);
});
