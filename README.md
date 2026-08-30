<div align="center">

<img src="internal/server/static/bazelgeuse.webp" alt="" width="120" height="120">

# bazel

**Your team's open pull requests, in one place — reviewed by the AI agents you choose.**

[![Go](https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![gh CLI](https://img.shields.io/badge/requires-gh%20CLI-181717?logo=github&logoColor=white)](https://cli.github.com)
[![Agents](https://img.shields.io/badge/agents-your%20Claude%20Code%20skills-f97316)](https://claude.com/claude-code)
[![Runs on](https://img.shields.io/badge/runs%20on-127.0.0.1%20only-8b9099)](#security-model)

</div>

---

Bazel is a small, single-user web app you run on your own machine. It collects
the open pull requests of the repositories you watch, hands the ones you pick to
an AI agent, and shows you the review — the agents working live, the report
rendered, the cost in tokens. Nothing reaches GitHub unless you say so.

There are no subcommands: **the binary is the server**. Repositories, agents and
reviews are all managed from the page.

## Highlights

- **Agents are your own skills.** The agent list starts empty; you build it from
  the [Claude Code skills](#agents-are-your-skills) installed on your machine.
- **Reviews outlive the tab.** Each review is a server-side job. Close the
  browser, come back later, it's still there.
- **A terminal per agent.** A fleet of lenses running in parallel shows up as
  one live pane each, not as one scrambled stream.
- **You are the gate to the PR.** Read the review first; publishing is a
  separate, explicit action.
- **Token spend is reported** for every run — per agent in the log, and as a
  total when the review lands.

## How it works

<div align="center">
  <img src="docs/flow.svg" alt="GitHub to the local bazel server, to a throwaway clone, to your agent, and back to you" width="940">
</div>

1. `gh pr view` brings in the metadata (title, author, branch, base, body).
2. The repository is cloned into a **throwaway directory** (`gh repo clone` with
   `--filter=blob:none` — full history, no blobs) and the PR is checked out.
3. Each agent of your choice runs **with that clone as its working directory**,
   receiving the prompt on stdin. A pipeline runs its agents one after another
   over the same clone — cloning once.
4. The clone is deleted at the end. `--keep` preserves it and the path shows up
   in the review footer.

A step that fails doesn't sink the review: it becomes a section with the error
and the rest carries on. Only when no agent returns anything does the whole
review fail.

Because the agent browses the checked-out code, the diff is **not** pasted into
the prompt — it is only downloaded if your template uses `{{diff}}`.

## Requirements

- Go 1.25+ (to build)
- [`gh`](https://cli.github.com), authenticated (`gh auth login`)
- `git`
- An AI agent on your `PATH` that reads a prompt on stdin — by default
  [`claude`](https://claude.com/claude-code) in [stream mode](#the-live-log),
  which is what feeds the live log
- The skills you want to use as review lenses, installed in `~/.claude/skills`

## Install

```sh
make install                      # builds into ~/.local/bin
make install PREFIX=/usr/local/bin
```

Or with Go:

```sh
go install github.com/beroni/bazel@latest
```

> The binary is called `bazel`, like Google's build tool. If you use both,
> rename one of them at install time (`-o ~/.local/bin/bz`).

## Quick start

```sh
bazel          # serves on 127.0.0.1:7777
bazel --open   # and opens the browser
```

On first run `~/.bazel/config.yaml` is created for you. Then, in the page:

1. Open **config** and add a repository (`owner/repo`).
2. In the same dialog, turn one of your installed skills into an **agent**.
3. Tick a PR, pick the agent, hit **revisar**.

| Flag | Effect |
| --- | --- |
| `--addr <host:port>` | where to listen (default `127.0.0.1:7777`) |
| `--jobs <n>` | concurrent reviews (default 2) |
| `--open` | open the browser |
| `--keep` | keep the throwaway PR clones |
| `--no-splash` | skip the opening animation |
| `--version` | version |

`--jobs` is what separates "reviewing two PRs" from "melting the laptop": every
review clones a repository and spawns an agent process.

## The web UI

A review takes minutes and no HTTP request survives that, so each one becomes a
**server-side job**. The browser gets an id immediately and the result arrives
over [SSE](https://developer.mozilla.org/docs/Web/API/Server-sent_events).

From the page you can:

- **tick PRs** and choose **which agent** runs over them;
- **filter the list**: by text (title, repo or author), by ownership
  (`todos` / `só meus`), by repository, and by review state — `sem review`,
  `✓ revisado`, `⟳ mudou desde o review`. With a filter on, the header counter
  becomes `12 de 92 PRs`. Filtering never unticks anything, but **revisar** only
  runs on what is currently visible;
- **collapse the list** — the `☰` button sits in the list itself and stays
  behind as a thin rail, so a review can take the full window; the choice is
  remembered in the browser;
- follow the queue, cancel a job, and watch the [live log](#the-live-log);
- **read the rendered review** and decide whether it goes to the PR — inline
  comments or a plain comment, see [Publishing](#publishing-to-the-pr);
- re-read older reviews saved on disk;
- add and remove watched repositories, and build your
  [agent list](#agents-are-your-skills) from the installed skills.

### What has been reviewed

Once a review finishes, the PR is **marked in the list** — and when it gets new
commits afterwards, the check turns into a warning:

```
#482  ✓ revisado 2h · publicado
#479  ⟳ mudou desde o review
```

The index lives in `<BAZEL_HOME>/reviews/.index.json`, keyed by the head commit
seen at review time.

## Agents are your skills

**An agent is one of your skills running over a PR.** That is why the list ships
empty: a factory list would only be right by accident, pointing at skills this
machine may never have had.

In **config**, the page lists what is actually installed — read from
`~/.claude/skills`, or from `skills_dir` in `config.yaml` — and every row turns
into an agent with one click:

```
skills instaladas · ~/.claude/skills
  /review-fleet     Runs a fleet of review lenses over one diff   [usar] [⇧ publica]
  /exploit-digger   Adversarial sweep of a diff                   [usar] [⇧ publica]
```

- **usar** creates the agent `review-fleet`, with the task
  `/review-fleet {{number}}`: it hands the review back to you to read.
- **⇧ publica** creates `review-fleet-post`, with `--post` and the prompt
  template that authorizes writing to GitHub — the page warns you before firing
  one of those.

The same skill can become both. In the list above each agent shows the skill it
calls, a **tornar padrão** button (the first one runs when you don't choose) and
**remover**:

```
review-fleet      padrão      ✓ /review-fleet
review-fleet-post ⇧ publica   ✓ /review-fleet
frota-em-série    pipeline    ✓ /senior-code-reviewer  ✓ /exploit-digger
post-report       usado ao publicar                    ✗ /post-report
```

The `✗` is the warning that matters: that agent calls a skill that is **not on
this machine** and would only fail at run time. Skills are usually symlinks into
the repository where you version them, and Bazel follows the links; the list is
read from disk every time you open the dialog, so installing a skill needs no
restart.

Everything the page does is written to `config.yaml`, and you can edit it by
hand for what the page doesn't offer — another model, another executable, your
own prompt template. See [Configuration](#configuration).

## The live log

The default args run the agent in stream mode:

```yaml
agent:
  command: claude
  args: [-p, --output-format, stream-json, --verbose, --allowedTools, "Read,Grep,Glob,Bash,Agent"]
```

In that mode stdout is a stream of JSON events: Bazel turns each one into a
readable line (the tool called, and the argument that says what it is doing) and
takes the final report from the result event.

**Every line is signed by whoever wrote it.** A fleet spawns its lenses inside
the same process, in parallel; Bazel ties each `Task` call to the sub-agent it
created and stamps the lines coming out of it:

```
review-fleet          | → Agent(senior-code-reviewer): precision review
review-fleet          | → Agent(exploit-digger): adversarial sweep
exploit-digger        | → Grep exec.Command
senior-code-reviewer  | → Read internal/agent/agent.go
lazy-senior-dev       | 40 lines the stdlib already does
```

**Each agent gets its own terminal**, with its own name, line count and scroll —
a fleet becomes four panes side by side, its own and one per lens. Two lenses of
the same type in parallel become `exploit-digger` and `exploit-digger 2`, not
one muddle.

The log is a window over the last **500 lines** per review, held in memory. It
does not travel over SSE: the page remembers where it stopped and fetches only
what is missing, once a second. The agent's stderr is included, in another color.

> If your `agent.args` is customized, Bazel leaves it alone — add
> `--output-format stream-json --verbose` to get the translated log.

## Token usage

When an agent finishes, its last log line says what the run cost:

```
review-fleet          | ✓ pronto em 4m12s · 1,8M tokens · $2.41
```

The total shows up **at the end**: on the queue card, under the agent name, and
in the footer of the report, right where you finish reading.

```
1,8M tokens · $2.41 · 252s
```

It is what the final `stream-json` event reports — input, output and cache
added up, **sub-agents included** (a fleet's three lenses are in there) — and in
a pipeline it is the sum of the steps. The same number goes into the header of
the saved markdown:

```
- Gasto: 1,8M tokens (entrada 12k · saída 84k · cache 1,7M) · $2.41
```

An agent that doesn't speak `stream-json` reports no spend, and the line simply
doesn't appear.

## Where reviews go

Every review lands in three places, in this order:

1. **The page** — markdown rendered in the right-hand pane.
2. **A file** — `<BAZEL_HOME>/reviews/<repo>-<number>-<date>.md`, with the PR
   header, the agent that ran (per-step timings in a pipeline) and the
   [token spend](#token-usage).
3. **The PR on GitHub** — only if you ask, and only after you have read it.

## Publishing to the PR

Three ways in, from the most deliberate to the most direct.

**1. Read, then publish** (the default). Run a review, read it on screen, then
click **publicar review inline**. That runs the `post_agent` — the `post-report`
skill — over a clone of the PR, with the markdown file you just read in the
prompt and the instruction **not to redo the review**: it publishes what is in
the file, with inline comments on the right lines, 👍 on what is already flagged
in the PR, and an all-clear when there is nothing to say. It becomes a job like
any other, with steps and a log.

**2. Paste as a comment** ("ou colar como comentário"). This is Bazel writing,
with no agent: the review markdown becomes a single comment, immediately. No
inline anchors, but no agent spend either.

**3. Publish directly**, skipping your reading: pick an agent marked `⇧` in the
selector before reviewing — the one you created with **⇧ publica**. It reviews
and publishes in the same pass.

Agents in paths 1 and 3 carry their own prompt template: the default one forbids
writing to GitHub, and theirs replaces that with explicit authorization. Any
agent of yours can do the same with `posts: true`, which is what makes the UI
mark it with `⇧` and ask before firing — publishing is a write on someone else's
PR. And if you ask to comment over a review the agent already published, Bazel
warns you first.

## Configuration

`~/.bazel/config.yaml` (or `$BAZEL_HOME/config.yaml`):

```yaml
repos:
  - acme/api-core
  - acme/web-app

authors:          # filter by PR author; empty means everyone
  - beroni

include_drafts: false

reviews_dir: ""   # empty = <BAZEL_HOME>/reviews
max_diff_bytes: 400000   # only used if the prompt has {{diff}}
skills_dir: ""    # empty = ~/.claude/skills

# The base every named agent inherits from.
agent:
  command: claude
  args: [-p, --output-format, stream-json, --verbose, --allowedTools, "Read,Grep,Glob,Bash,Agent"]
  checkout: true          # clone the repo and check the PR out first
  timeout_seconds: 1800
  prompt: |-
    {{task}}
    ...

# The lenses the selector offers. Starts empty — the page fills it from your
# installed skills. The first one is the default.
agents: []

# Sequences run over the same clone.
pipelines: []

# Who takes an already-read review to the PR.
post_agent:
  name: post-report
  task: /post-report {{review_file}}
  posts: true
```

### Agents and pipelines

An agent only declares **what changes**: its `task` goes into the `{{task}}` of
`agent.prompt`, and `command`, `args`, `checkout` and `timeout_seconds` are
inherited from the `agent` block when left out. `prompt` replaces the whole
template.

```yaml
agents:
  - name: review-fleet
    description: three lenses, deduplicated into one verdict
    task: /review-fleet {{number}}
  - name: exploit-digger
    description: adversarial recall, class by class
    task: /exploit-digger {{number}}
  # This one publishes on its own: `posts` is what makes the UI warn first.
  - name: review-fleet-post
    task: /review-fleet {{number}} --post
    posts: true
  # A lens can run on another model, or another executable entirely.
  - name: rapidinha
    task: /senior-code-reviewer {{number}}
    args: ["-p", "--model", "claude-haiku-4-5-20251001", "--allowedTools", "Read,Grep,Glob,Bash"]
    timeout_seconds: 600

pipelines:
  - name: frota-em-série
    description: the three lenses one at a time, each in its own process
    steps: [senior-code-reviewer, exploit-digger, lazy-senior-dev]
```

A **pipeline** chains agents by name, in order, over the same clone; the report
comes out with one section per step. A step pointing at an agent that doesn't
exist is skipped.

With no agents at all, the **revisar** button stays disabled and the page tells
you what is missing. If you wrote your own `agent.prompt` and have no `agents:`,
the selector shows a single choice — the bare `agent` block, which is how Bazel
behaved before the selector existed.

### Prompt placeholders

`{{task}}`, `{{repo}}`, `{{number}}`, `{{title}}`, `{{author}}`, `{{url}}`,
`{{branch}}`, `{{base}}`, `{{body}}`, `{{workdir}}`, `{{diff}}`.

`{{task}}` is the chosen agent's instruction — the only thing that changes from
one lens to the next. A template without `{{task}}` gets the instruction
prepended on the first line.

The `post_agent` gets two more: `{{review_file}}`, the path of the markdown you
read, and `{{review}}`, its text.

`{{diff}}` is the only one that costs an extra call to GitHub — if it isn't in
the template, the diff is never downloaded.

### Using another agent CLI

Anything that reads a prompt on **stdin** and writes markdown to **stdout**
works. With `checkout: true` it runs inside the PR clone, and whatever it writes
goes to the [live log](#the-live-log) line by line.

```yaml
# Claude Code on a specific model
agent:
  command: claude
  args: ["-p", "--model", "claude-opus-5", "--allowedTools", "Read,Grep,Glob,Bash,Agent"]

# Codex CLI
agent:
  command: codex
  args: ["exec", "-"]
```

> `claude -p` denies every permission that isn't granted, silently. That is why
> the default args carry `--allowedTools Read,Grep,Glob,Bash,Agent` — without
> `Agent` a fleet can't spawn its lenses, without `Bash` none of them can work
> out the scope. The agent runs in a throwaway clone and review skills are
> read-only: they report, they don't fix.

## Environment variables

| Variable | Effect |
| --- | --- |
| `BAZEL_HOME` | config directory (default `~/.bazel`) |
| `BAZEL_NO_SPLASH` | disables the opening animation |
| `NO_COLOR` / `CI` | also disable the animation |

## Security model

**Single-user by construction, and the port is local on purpose.** The server
uses the machine's already-authenticated `gh` — anyone who reaches it can make
it clone repositories and run an agent with `Bash` enabled. So it listens on
loopback, rejects a `Host` that isn't local (blocking DNS rebinding) and rejects
`POST` from another origin (blocking a random tab from firing reviews in your
name). Don't put this behind a public IP without authentication in front.

## Development

```sh
make          # build ./bazel
make run      # serve the web UI and open the browser
make check    # fmt + vet + test, before committing
make help     # every target
```

The front end (`internal/server/static/`) is embedded in the binary with
`go:embed` — no build step, no CDN: the page works offline.

Packages: `server` (HTTP, job queue, SSE), `agent` (runs the agents and
translates the stream), `config`, `gh` (talks to the `gh` CLI), `workspace` (the
throwaway clone), `store` (saved reviews and the reviewed index), `skills`
(discovers installed skills) and `splash` (the egg).

<div align="center">
<sub>Starting <code>bazel</code> hatches a Bazelgeuse bomb egg that cracks, heats up and detonates into the logo.<br>
Turn it off with <code>--no-splash</code> or <code>BAZEL_NO_SPLASH=1</code>.</sub>
</div>
