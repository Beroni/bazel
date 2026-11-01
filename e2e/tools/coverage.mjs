#!/usr/bin/env node
// coverage.mjs adds up the V8 coverage of app.js from every test of a run
// (E2E_COVERAGE=1, written by harness/fixtures.ts) and prints what the e2e
// suite reaches: lines, functions, and the largest functions it never calls.
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const dir = path.join(path.dirname(fileURLToPath(import.meta.url)), '..', '.out', 'coverage');
const files = fs.existsSync(dir) ? fs.readdirSync(dir) : [];
if (!files.length) {
  console.error('no coverage: run `make e2e-cover`');
  process.exit(1);
}

let source;
let hit; // per character: covered by any test
const fns = new Map(); // "name@start" → { name, start, end, called }
for (const f of files) {
  for (const entry of JSON.parse(fs.readFileSync(path.join(dir, f), 'utf8'))) {
    source ??= entry.source;
    hit ??= new Uint8Array(source.length);
    const run = new Uint8Array(source.length);
    // V8's ranges nest, outer first: an inner count overrides the outer one.
    for (const fn of entry.functions) {
      for (const r of fn.ranges) run.fill(r.count > 0 ? 1 : 0, r.startOffset, r.endOffset);
      const outer = fn.ranges[0];
      if (fn.functionName) {
        const key = `${fn.functionName}@${outer.startOffset}`;
        const known = fns.get(key) ?? { name: fn.functionName, start: outer.startOffset, end: outer.endOffset, called: false };
        known.called ||= outer.count > 0;
        fns.set(key, known);
      }
    }
    for (let i = 0; i < run.length; i++) if (run[i]) hit[i] = 1;
  }
}

let lines = 0;
let covered = 0;
const lineOf = (offset) => source.slice(0, offset).split('\n').length;
let at = 0;
for (const text of source.split('\n')) {
  const code = text.trim();
  if (code && !code.startsWith('//')) {
    lines++;
    if (hit[at + text.indexOf(code)]) covered++;
  }
  at += text.length + 1;
}
const all = [...fns.values()];
const called = all.filter((f) => f.called);
const pct = (a, b) => `${((a / b) * 100).toFixed(1)}%`;
console.log(`app.js, ${files.length} pages: lines ${covered}/${lines} (${pct(covered, lines)}), named functions ${called.length}/${all.length} (${pct(called.length, all.length)})`);
const missed = all.filter((f) => !f.called).sort((a, b) => (b.end - b.start) - (a.end - a.start)).slice(0, 15);
if (missed.length) console.log(`never called:\n${missed.map((f) => `  ${f.name} (app.js:${lineOf(f.start)}, ${f.end - f.start} bytes)`).join('\n')}`);
