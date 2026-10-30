# Running one lens on its own

The procedure the three single-lens skills share (`bazel-senior-code-reviewer`,
`bazel-exploit-digger`, `bazel-lazy-senior-dev`). Each `SKILL.md` adds only what is specific to
its lens. `headless.md` applies throughout.

## Step 1 — Resolve the scope

Section 2 of `headless.md`. Empty → failure report (its section 5), stop.

## Step 2 — Run the lens

Hand the lens the scope block below, verbatim:

```
Scope for this review (resolved by the orchestrator — use exactly this, do NOT resolve it
again):

  Command: git diff origin/develop...HEAD
  Files (N): src/api/users.ts, src/lib/auth.ts, ...
  PR #123 — "<title>" — base: develop
  PR description: <body, or "no description">

Review that scope with your lens and return ONLY the JSON of your output contract.

Write every prose field of that JSON in English, regardless of the language of this prompt,
of the repo, or of the user's request.
```

- **With the `Agent` tool**: one `Agent` call with the lens's `subagent_type`
  (`bazel-<lens>`) and `run_in_background: false`, and wait for it in the same turn.
- **Without it**, or if the agent type is unknown: read the lens's agent file
  (`.claude/agents/bazel-<lens>.md`, also at `.claude/skills/bazel-<lens>/agents/`) and do the
  pass yourself, following it to the letter, down to the JSON.

Prose or malformed JSON back → ask again (or redo the pass). Never guess its content, and
never replace the lens with a generic review of your own.

## Step 3 — Print the report

Format: `.claude/skills/bazel-review-fleet/references/report-format.md`. For a single lens it
says:

- the H1 is the lens's (`# Senior Code Reviewer — …`, `# Exploit Digger — …`,
  `# Lazy Senior Dev — …`);
- `lenses_ran` carries only this lens; `lenses_skipped` names the other two with
  `(single-lens run)` as the reason;
- a lens writes only its own sections (the spec's "Who may emit what" table).

Copy `severity` and `class` verbatim from the JSON. `findings: []` is a legitimate result: the
`**Verdict:**` line with `no findings` already says so.

The verdict follows what is in the report, with the fleet's rule (`bazel-review-fleet`, Step 5):
`request_changes` for a `critical` or `major` finding, `comment` for anything else in it,
`approve` for nothing — even when the lens's own `verdict` said otherwise, for instance after
`bazel-lazy-senior-dev` dropped a load-bearing cut.

Write it to a scratch file and validate it (section 3 of `headless.md`), fix until it
passes, and print its content as your final output, with nothing around it.
