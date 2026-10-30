# Selectors and the data-testid contract

All selectors live in `e2e/harness/ui.ts`. Each one tries `getByTestId(id)`
first and falls back to role/visible text, so the suite keeps working before,
during and after the UI adopts the ids. Never select by CSS class.

Rules for the UI side:

- `data-testid` names are stable API: rename only together with `ui.ts`.
- Repeated items carry the id **and** a data attribute with their identity
  (`data-pr-key`, `data-job-id`, `data-state`…), so tests do not depend on order.
- Ids go on the element the user interacts with (the `<button>`, the `<input>`),
  containers get their own ids.

## Contract

Status: **implemented on master since #30.**
`harness/ui.ts` uses the testid when the page has it and master's role/text
otherwise; the whole suite passes on both. P0 = used by the P0 tests.

Differences on the redesign, already handled by the helpers:

- No `job-publish`, `job-comment`, `job-choose`, `job-keep`: those actions live
  in the review pane (`publish-inline`, `paste-comment`, `keep-here`).
- `job-cancel` is in the job's detail header, not on the card (`cancelJob()`).
- `job-card` has `role="group"`, an `aria-label` with PR, title and state,
  `tabindex=0`; Enter/Space open it.
- `pr-review-filter` is a group of chips (`data-review=all|none|reviewed|changed`),
  not a `<select>` (`filterByReview()`).
- `pr-row-check` has `aria-label="tick owner/repo#n"`; `pr-owner-filter` (#25)
  and `conn-status` (`data-state=open|closed`) exist; `quota` has `data-level`.
- Compact report (c2614d9/60ba42e): each finding is a `finding-row`
  (`data-finding-index`, `data-severity` crit|high|med|low|none, `data-open`)
  with a `finding-toggle` (`aria-expanded`); only the first starts open. Call
  `expandFindings(viewer)` before asserting on text inside a finding's body.
  Labels changed: "↗ Publish" and "As comment" (testids unchanged);
  `pick-summary` reads "N of M findings".
- Sidebar/header (79f3807): the PR filters (`pr-scope`, `pr-hide-drafts`,
  `pr-owner-filter`, `pr-repo-filter`) live in `pr-filter-pop`, opened by
  `pr-filter-menu` (`aria-expanded`) — call `openFilters(page)` first.
  `pr-count`, `spend-total`, `quota` and `whoami` live in the `#meter`
  popover (hover/focus); `toHaveText` works as is, `toBeVisible` needs
  `showMeters(page)`. `quota-short` reads "N% of session used".
- `pr-review-tag` texts: "reviewed now" / "reviewed 2h", "changed",
  "reviewing" (data-state unchanged, plus `reviewing`); one tag per row.
- Card state, agent, "✓ published" and tokens are sr-only text (testids
  `job-state`, `job-published`, `job-tokens` unchanged).
- PR page (Overview): `pr-page` (`data-pr-key`, `data-loaded`), `pr-activity`,
  `pr-files`, `pr-reviewer` (`data-login`, `data-state`), `pr-label`, `pr-check`
  (`data-name`, `data-state`), `pr-error` (stays until Retry) and `retry`, the
  Retry of any load error. A job of a PR shows in `pr-activity`; a history job
  says "is writing / wrote the history with" and gives no "reviewing" tag.
- History tab: `history` (`data-pr-key`), `history-generate`,
  `history-regenerate`, `history-decision` (`data-n`).
- `review-more`: the `<details>` "More from the report". `data-publishes=true`:
  open, labelled "also goes to the PR"; `false`: closed, no label.
- A markdown image becomes a link "image: alt" in the PR body, reviews and
  History: there is no `<img>`. The publish confirm says "Goes to the PR: N of M
  findings (whole bodies)…". Before `/api/prs` answers the list says it is
  loading, never "no open PRs".
- `review-hidden`: warning on top of the Report with the text only the markdown
  has (`[//]: # (…)`, unreferenced notes), each piece in a `<pre>`; the publish
  confirm counts them. A `<!-- finding … -->` stays hidden only in the exact
  shape of `report-format.md` (no quotes, no `id`): use
  `<!-- finding severity=minor path=a.go line=3 -->` in fixtures.
- `history-custom`: notice in the History tab and the history job's header when
  the step is not Bazel's own (`history_custom` in `/api/state`).
- `log-pane` marks each agent in the Live log column (`data-agent`, `all`).
- Since #25 the tags read "✓ just now" and "⟳ changed" (master: "✓ reviewed
  just now", "⟳ changed since review"); tests match both.

### Header

| testid | element | extra attrs | P0 |
| --- | --- | --- | --- |
| `whoami` | `@login` | | ✓ |
| `pr-count` | "5 PRs" / "1 of 5 PRs · 1 ticked" | | ✓ |
| `spend-total` | session tokens · $ | `data-partial` | |
| `quota` | claude session/week | `data-level="ok|warn|crit"` | |
| `refresh` | refresh button | | |
| `nav-dashboard`, `nav-config` | page switch | | |
| `banner` | message bar; user-action errors are an alert (class `alert`) that stays until ✕ or the next action | | |
| `banner-dismiss` | the ✕ of an alert (`aria-label="dismiss this message"`) — proposed; the role/name fallback works today | | |
| `conn-status` | SSE dot | `data-state="open|closed"` | |

### PR list

| testid | element | extra attrs | P0 |
| --- | --- | --- | --- |
| `pr-filter` | text filter input | | ✓ |
| `pr-scope` | everyone/mine segmented | buttons: `data-scope` | |
| `pr-hide-drafts` | checkbox | | |
| `pr-repo-filter`, `pr-review-filter` | selects | | |
| `pr-row` | one PR row | `data-pr-key="owner/repo#n"`, `data-selected` | ✓ |
| `pr-row-check` | the row's checkbox (needs an accessible name, e.g. `aria-label="tick #101"`) | | |
| `pr-review-tag` | ✓ reviewed / ⟳ changed | `data-state="reviewed|changed|published"` | |
| `pr-select-all` | select all | | ✓ |
| `agent-select` | agent selector | | ✓ |
| `run-review` | run button | | ✓ |
| `toggle-prs` | ☰ | | |

### Queue and cards

| testid | element | extra attrs | P0 |
| --- | --- | --- | --- |
| `tab-queue`, `tab-saved`, `queue-count` | tabs | | |
| `job-card` | one card | `data-job-id`, `data-state`, `data-pr-key` | ✓ |
| `job-state` | state label | | |
| `job-tokens` | `~412k tokens` | `data-partial` | |
| `job-step` | step row | `data-step`, `data-state` | |
| `job-cancel` | cancel | | ✓ |
| `job-remove` | ✕ | | |
| `job-publish`, `job-comment` | card publish buttons | | |
| `job-choose`, `job-keep` | publish-step wait: choose / keep it here | | |
| `job-published` | ✓ published | | |

### Review pane

| testid | element | extra attrs | P0 |
| --- | --- | --- | --- |
| `review-viewer` | the pane | `data-job-id` or `data-saved` | ✓ |
| `review-body` | rendered markdown (publishable part) | | |
| `review-context` | "stays in Bazel" part | | |
| `finding-pick` | report checkbox | `data-finding-index` (the server's skip index) | ✓ |
| `finding-pick-all`, `finding-pick-none`, `pick-summary` | | | |
| `publish-inline` | publish inline review | | ✓ |
| `paste-comment` | or paste as a comment | | ✓ |
| `keep-here` | keep it here | | |
| `already-published` | ✓ already on the PR | | ✓ |
| `post-error` | failed to publish | | |
| `review-footer` | saved to … · clone kept at … | | |
| `live-log` | log panel | | |
| `log-pane` | one terminal | `data-agent` | |
| `log-line` | one line | `data-stream="stdout|stderr"` | |

### Saved tab

| testid | element | extra attrs |
| --- | --- | --- |
| `saved-row` | one saved review | `data-name` (file name) |
| `saved-publish`, `saved-comment` | buttons | |

### Config page

| testid | element | extra attrs |
| --- | --- | --- |
| `repo-input`, `repo-add` | add repo form | |
| `repo-row`, `repo-remove` | watched repo | `data-repo` |
| `agent-row` | agent | `data-agent`, `data-default`, `data-missing-skill` |
| `agent-default`, `agent-remove`, `agent-publishable` | row actions | |
| `skill-row`, `skill-use`, `skill-publishes` | installed skill | `data-skill` |
| `pipeline-tray`, `pipeline-chain`, `pipeline-step` | builder | `data-step` |
| `pipeline-name`, `pipeline-create`, `pipeline-row`, `pipeline-remove` | | `data-pipeline` |
| `config-download`, `config-yaml` | file | |
| `stray-warning` | stray command warning | |
