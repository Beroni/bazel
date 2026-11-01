#!/usr/bin/env node
// stream.mjs builds fake-agent scenarios for skills.
//
//   node e2e/tools/stream.mjs synth <spec.json>   → claude-stream scenario on stdout
//   node e2e/tools/stream.mjs scrub <raw.jsonl>   → a real claude stream, made safe to commit
//
// Specs dropped in e2e/fixtures/agent/specs/*.json are synthesized by the
// harness before every run into e2e/.out/scenarios/<name>.jsonl, so a test
// uses them as FAKE_AGENT_SCENARIO=<name> with nothing else to do.
//
// Spec format (all fields optional except report/reportFile):
// {
//   "model": "claude-opus-5-5",
//   "tools": ["Read", "Grep", "Agent"],
//   "rateLimit": { "session": 0.42, "week": 0.10 },
//   "steps": [
//     { "tool": "Read", "input": { "file_path": "main.go" } },
//     { "text": "a line the main agent says" },
//     { "agent": "exploit-digger", "description": "adversarial sweep",
//       "steps": [ { "tool": "Grep", "input": { "pattern": "exec" } }, { "text": "…" } ] },
//     { "toolError": "Read", "input": { "file_path": "gone.go" }, "error": "file not found" },
//     { "directive": "#!wait release-me" }
//   ],
//   "report": "# Review\n\n## Findings\n\n### F1 …",   // or "reportFile": "my-skill.md" (next to the spec)
//   "usage": { "input": 1000, "output": 200, "cacheRead": 0, "cacheWrite": 0, "costUSD": 0.12 },
//   "durationMs": 3000,
//   "error": "error_max_turns"                        // ends with is_error instead of success
// }
import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const ev = (o) => JSON.stringify(o);

export function synth(spec, specDir = '.') {
  const model = spec.model || 'claude-opus-5-5';
  const lines = [];
  let n = 0;
  const id = () => `tu_${++n}`;
  const u = (i = 10, o = 10) => ({ input_tokens: i, output_tokens: o, cache_read_input_tokens: 0, cache_creation_input_tokens: 0 });
  const msg = (content, parent, usage = u()) =>
    ev({ type: 'assistant', message: { model, content, usage }, parent_tool_use_id: parent ?? null });

  lines.push(ev({ type: 'system', subtype: 'init', tools: spec.tools || ['Read', 'Grep', 'Glob', 'Bash', 'Agent'], model }));
  if (spec.rateLimit) {
    const at = (h) => Math.floor(Date.UTC(2030, 0, 1) / 1000) + h * 3600;
    lines.push(ev({ type: 'rate_limit_event', rate_limit_info: { status: 'allowed', unifiedWindows: {
      five_hour: { utilization: spec.rateLimit.session ?? 0, resetsAt: at(5) },
      seven_day: { utilization: spec.rateLimit.week ?? 0, resetsAt: at(168) },
    } } }));
  }

  const walk = (steps, parent) => {
    for (const s of steps || []) {
      if (s.directive) {
        lines.push(s.directive);
      } else if (s.text !== undefined) {
        lines.push(msg([{ type: 'text', text: s.text }], parent));
      } else if (s.tool) {
        lines.push(msg([{ type: 'tool_use', id: id(), name: s.tool, input: s.input || {} }], parent));
      } else if (s.toolError) {
        const tid = id();
        lines.push(msg([{ type: 'tool_use', id: tid, name: s.toolError, input: s.input || {} }], parent));
        lines.push(ev({ type: 'user', message: { content: [{ type: 'tool_result', tool_use_id: tid, is_error: true, content: s.error || 'error' }] }, parent_tool_use_id: parent ?? null }));
      } else if (s.agent) {
        const tid = id();
        lines.push(msg([{ type: 'tool_use', id: tid, name: 'Agent', input: { subagent_type: s.agent, description: s.description || s.agent } }], parent));
        if (s.prompt) lines.push(ev({ type: 'user', message: { content: [{ type: 'text', text: s.prompt }] }, parent_tool_use_id: tid }));
        walk(s.steps, tid);
      } else {
        throw new Error('spec step with nothing to do: ' + JSON.stringify(s));
      }
    }
  };
  walk(spec.steps, null);

  const report = spec.reportFile ? fs.readFileSync(path.resolve(specDir, spec.reportFile), 'utf8') : spec.report;
  if (report === undefined) throw new Error('spec needs report or reportFile');
  const us = spec.usage || {};
  const usage = { input_tokens: us.input || 0, output_tokens: us.output || 0, cache_read_input_tokens: us.cacheRead || 0, cache_creation_input_tokens: us.cacheWrite || 0 };
  const result = {
    type: 'result', subtype: spec.error || 'success', is_error: !!spec.error,
    duration_ms: spec.durationMs ?? 1000, result: report, usage,
  };
  if (us.costUSD) result.total_cost_usd = us.costUSD;
  if (us.input || us.output || us.cacheRead || us.cacheWrite) {
    result.modelUsage = { [model]: { inputTokens: usage.input_tokens, outputTokens: usage.output_tokens, cacheReadInputTokens: usage.cache_read_input_tokens, cacheCreationInputTokens: usage.cache_creation_input_tokens, costUSD: us.costUSD || 0 } };
  }
  lines.push(ev(result));
  return lines.join('\n') + '\n';
}

// scrub keeps what Bazel reads and drops what identifies a person or machine.
export function scrub(raw, { home = process.env.HOME || '' } = {}) {
  const out = [];
  const clean = (s) => {
    if (typeof s !== 'string') return s;
    let t = s;
    if (home) t = t.split(home).join('/home/e2e');
    t = t.replace(/\/(Users|home)\/[^/\s"]+/g, '/home/e2e');
    t = t.replace(/\/(private\/)?(var\/folders|tmp)\/[^\s"]*/g, '/tmp/e2e-clone');
    t = t.replace(/[\w.+-]+@[\w-]+\.[\w.]+/g, 'e2e@example.invalid');
    t = t.replace(/\b(ghp|gho|ghs|github_pat|sk-ant|sk)-?[A-Za-z0-9_-]{16,}\b/g, '<redacted-token>');
    return t;
  };
  const deep = (v) => {
    if (Array.isArray(v)) return v.map(deep);
    if (v && typeof v === 'object') {
      const o = {};
      for (const [k, x] of Object.entries(v)) {
        if (['session_id', 'uuid', 'request_id', 'cwd', 'apiKeySource', 'mcp_servers', 'slash_commands', 'plugins', 'agents', 'skills', 'output_style'].includes(k)) continue;
        o[k] = deep(x);
      }
      return o;
    }
    return clean(v);
  };
  for (const [i, line] of raw.split('\n').entries()) {
    if (!line.trim()) continue;
    // A capture is data. The fake agent runs `#!` lines, so one that came in a
    // capture would be its author's code on this machine: nothing to keep.
    if (line.trimStart().startsWith('#!')) throw new Error(`line ${i + 1} is a replay directive, and a capture holds none: ${line.slice(0, 60)}`);
    let e;
    try {
      e = JSON.parse(line);
    } catch {
      out.push(clean(line));
      continue;
    }
    // Tool results are noise in a fixture and the likeliest place for secrets.
    for (const b of e?.message?.content || []) {
      if (b?.type === 'tool_result') {
        const text = typeof b.content === 'string' ? b.content : JSON.stringify(b.content);
        b.content = b.is_error ? text.slice(0, 200) : '(scrubbed)';
      }
    }
    if (e.type === 'system' && Array.isArray(e.tools)) e.tools = e.tools.filter((t) => !String(t).startsWith('mcp__'));
    out.push(JSON.stringify(deep(e)));
  }
  return out.join('\n') + '\n';
}

/** Synthesizes every specs/*.json under specsDir into outDir/<name>.jsonl. */
export function synthAll(specsDir, outDir) {
  fs.mkdirSync(outDir, { recursive: true });
  if (!fs.existsSync(specsDir)) return [];
  const made = [];
  for (const f of fs.readdirSync(specsDir).filter((x) => x.endsWith('.json')).sort()) {
    const spec = JSON.parse(fs.readFileSync(path.join(specsDir, f), 'utf8'));
    const name = f.replace(/\.json$/, '');
    fs.writeFileSync(path.join(outDir, name + '.jsonl'), synth(spec, specsDir));
    made.push(name);
  }
  return made;
}

if (process.argv[1] && fileURLToPath(import.meta.url) === path.resolve(process.argv[1])) {
  const [cmd, file] = process.argv.slice(2);
  if (!['synth', 'scrub'].includes(cmd) || !file) {
    console.error('usage: stream.mjs synth <spec.json> | scrub <raw.jsonl>');
    process.exit(2);
  }
  const text = fs.readFileSync(file, 'utf8');
  process.stdout.write(cmd === 'synth' ? synth(JSON.parse(text), path.dirname(file)) : scrub(text));
}
