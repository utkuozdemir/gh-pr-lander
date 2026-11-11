# gh-pr-lander — agent guide

You (the agent) work on this repository. `CLAUDE.md` and `GEMINI.md` just
import this file, so it is the one place to edit. **Part of your job, every
time, is to keep this file accurate and richer than you found it.** When you learn something
non-obvious about the code, GitHub's API behavior, a design decision, a gotcha,
or a user preference, add it here. Prefer updating an existing section over
appending a new one. Treat this file as your long-term memory for the project:
the better it is, the faster and safer every future change becomes. Keep it
honest — if something here turns out to be wrong, fix it.

## What this tool is

`gh-pr-lander` lands its owner's GitHub pull requests in a workflow with
signed commits and fast-forward merges. It has a terminal (Bubble Tea) front
end and a server with a page. Nothing in it is specific to one company or
organization: which organizations to watch is `--orgs`, and with none given it
watches every PR of its owner on GitHub. In the workflow it serves every
commit is GPG-signed with a YubiKey, the GitHub
merge button is never used, and merges are fast-forwards of the base branch to
the PR's head commit after the author has rebased locally onto the latest base.
That means constant rebasing, watching CI, and racing to merge before someone
else merges and your PR goes stale. This tool watches all your open PRs across
configured orgs and gives you one-key rebase / merge / rerun, plus automerge and
autorerun automation.

User-facing details live in `README.md`. This file is about *how the code
works* and *why*.

## Architecture

The code is split into small packages under `internal/`. The dependency
direction is strict and acyclic:

```
main  ──>  tui     ──>  engine ──> github ──> pr
      │         │        ├──>  store
      │         │        ├──>  notify
      │         │        └──>  config
      │         └──>  git ──> pr
      └──>  server  ──>  engine
        github ──> pr,  config-independent
```

- **`internal/pr`** — the domain model. `PR` struct, the status enums
  (`BaseStatus`, `CIStatus`, `ReviewStatus`, `OverallStatus`), `CheckSummary`,
  `ComputeOverallStatus`, and `Sort`. Pure, deterministic, no I/O, no API
  shapes. This is the lingua franca every other package speaks.
- **`internal/github`** — the *only* package that talks to the GitHub API
  (GraphQL for PR search, REST for ref updates / Actions / branch deletion). It
  owns the raw API JSON shapes (`types.go`) and the mapping from those shapes
  into `pr.PR` (`status.go`). Nothing outside this package should ever see a
  `prNode` or other API struct. Also hosts the **`Poller`** (see below).
- **`internal/git`** — builds the bash rebase script. The rebase deliberately
  runs as a real foreground process (`tea.ExecProcess`) because YubiKey signing
  needs an interactive touch/PIN prompt.
- **`internal/store`** — loads/saves `db.yaml` (which PRs have automerge /
  autorerun on, and the last autorerun time). Atomic write via temp+rename.
  Everything else (retry counts, etc.) is derived live from the API so the file
  stays tiny.
- **`internal/notify`** — desktop notifications (macOS osascript + terminal
  bell). Enable/disable is process-global (`atomic.Bool`). Each kind of
  notification is an `Event` (title + sound from `/System/Library/Sounds`),
  all in one table in `notify.go`, each with a distinct sound. When automation
  is on for a PR, the transition it will handle does not notify, only its
  outcome does (see `checkTransitions`).
- **`internal/config`** — the resolved `Config` value, built once in `main`.
- **`internal/engine`** — everything the tool does with PRs that needs no user
  interface and no signing: the automerge and autorerun switches (`auto.go`),
  both settling guards, merged/closed tracking and transition notifications
  (`merged.go`), the GitHub calls behind merge and re-run (`work.go`), and the
  checks before a user action (`actions.go`). `State` is plain data with
  synchronous methods and starts no goroutines. A GitHub call is handed back
  as a `Work` func for the front end to run, and its `Result` goes into the
  matching `...Done` method. This is the part a second front end reuses.
- **`internal/tui`** — the Bubble Tea front end: model, update loop, views,
  styles, and what needs the YubiKey (rebase and the autorebase queue in
  `rebase.go`). The model embeds `*engine.State`, wraps each `engine.Work` in
  a `tea.Cmd` (`workCmd`), and turns results into status lines.
- **`internal/server`** — the front end with no terminal (`gh-pr-lander
  serve`): a fetch loop that feeds the engine and starts what automation asks
  for, plus the page (`page.html`, Go templates and an embedded htmx file).
  See "The server and the page" below.
- **`main.go`** — thin wiring only: parse flags, build the client and poller,
  run the program and the poller together under an errgroup. `serve.go` does
  the same for the server.

### Interfaces live on the consumer side

The engine and the TUI do not depend on `*github.Client` directly. The engine
declares a narrow `Client` interface (`internal/engine/engine.go`, aliased as
`tui.Client`) listing only the methods they use;
`*github.Client` satisfies it structurally. Same pattern for the poller:
`github.PRFetcher` is the one-method slice the poller needs. This is what makes
the TUI and poller unit-testable with fakes and no network. **When you add a
new GitHub call used by the engine or the TUI, add it to both `*github.Client`
and the `engine.Client` interface.**

### Responsiveness: the background Poller (issue #2)

Polling used to be UI-driven: a `tickMsg` every interval kicked off a fetch, and
effects of an action only showed up on the next tick. Now `github.Poller` runs
in its own goroutine (`Poller.Run(ctx)`), fetches immediately on start, then on
every interval tick **and** on every `Refresh()` request, and streams
`FetchResult`s over a channel. The TUI consumes them with `waitForResultsCmd`,
which blocks on the channel and re-issues itself after each result — a
continuous update stream.

After any action (merge, rerun, rebase, toggle automerge) the model calls
`refresh()` (wired to `Poller.Refresh`) so the new state is fetched right away
instead of waiting for the interval. `Refresh()` is non-blocking and coalesces
(buffered channel of size 1). Short follow-up refreshes at 15s/30s
(`delayedRefreshCmd`) cover GitHub's own propagation lag (e.g. GitHub
taking a moment to mark the PR merged after its base branch moved).

### Responsiveness: adaptive polling (issue #5)

**Why not push?** Issue #5 asked whether GitHub can *push* events (websocket/SSE)
so we react the instant CI passes instead of polling. The answer, after
investigation, is **no usable mechanism exists for a local CLI**:

- **Webhooks** push events but need a public HTTP endpoint. A laptop tool has
  none, and tunnels (smee.io, ngrok) are fragile and heavyweight for personal
  use.
- There is **no documented websocket/SSE event stream** in the REST or GraphQL
  API. The web UI's `wss://alive.github.com` is undocumented, uses session-cookie
  auth, and won't work with a PAT — don't rely on it.
- The **Notifications API** is itself polling (it returns an `X-Poll-Interval`
  header telling you how often to poll). No win.

So polling is the only option. What we *can* do is **poll adaptively**:
`Poller.nextInterval` shortens the next tick while any watched PR is
*transitional* (`anyTransitional`: CI running, or base/mergeability unknown), so
a CI pass/fail is caught within seconds, and relaxes back to the configured
interval when everything is idle. The fast cadence is a quarter of the
configured interval, floored at `minActiveInterval` (15s) so we don't burn rate
limit and capped so it's never slower than the normal interval. On a fetch error
`prs` is nil → normal interval, so an outage backs off instead of fast-spinning.
Polling is inherently retry-resilient: each cycle is independent, a failed fetch
just streams an error and the next cycle tries again. Tested purely via
`nextInterval`/`anyTransitional` (no timing-based flakiness).

### Structured concurrency (required)

Goroutines use structured concurrency — never fire-and-forget. The poller is run
under `errgroup` in `main` and the program **waits** for it (`g.Wait()`) after
the TUI exits and the context is cancelled. Inside `github`, per-org PR fetches
run in parallel via `errgroup` and Actions-API enrichment runs with a bounded
`errgroup` (`SetLimit(annotateConcurrency)`); both `Wait()` before returning.
Each parallel worker writes only its own slice index, so no locking is needed.
If you add concurrency, follow this: take a `context`, use `errgroup`, `Wait`,
and propagate cancellation.

## The server and the page

**Why it exists.** `/m` could be commented from a phone. The ref update can
only be sent from where the tool runs, so the owner lost merging while away
from the laptop. The server is the tool running always-on (a home cluster) with
a page to ask it for things. See `docs/adr/0001` for why the request goes
through a page and not through a mark on the PR, and `GLOSSARY.md` for the
words (terminal app, server, page).

**What it does and does not.** Everything that needs no signing: automerge,
autorerun, merge now, re-run, deleting the owner's fork branch after a merge.
Never a rebase, and it has no autorebase queue.

**How it uses the engine.** `Server` owns one `engine.State` behind a
`sync.Mutex`. Two things touch it: the fetch loop (`Run` → `handleFetch`) and
the HTTP handlers. GitHub calls always run outside the lock: the engine hands
back a `Work`, the server runs it, then takes the lock again to feed the result
into `finish`. Automation's work runs in goroutines tracked by `works`
(`sync.WaitGroup`), and `Run` waits for them on shutdown, so a merge in flight
finishes. A merge or re-run asked for from the page runs inside the request, so
the page can show the real outcome.

**Things that are deliberate:**

- **The terminal app and the server do not know about each other.** Each has
  its own `db.yaml` (the server's default data dir is
  `~/.gh-pr-lander/server` so the two never share a file). Both acting on
  one PR is safe: a second non-forced ref update changes nothing, and a
  re-run started by the other one is covered by the "re-runs started
  elsewhere" guard.
- **Only the owner's own PRs.** `handleFetch` drops taken-over PRs
  (`TakenOverFrom != ""`). Branch protection often does not
  dismiss approvals on a new push, so a contributor could push a commit to an
  approved PR and unattended automerge would merge it unseen.
- **Every form carries the head SHA the page showed** and `find` refuses the
  action if the PR has moved on. A page left open on a phone can be hours old,
  and "merge now" must not land on a commit the owner has not looked at.
- **No login yet** (issue #9). The page is meant for a LAN or VPN only.
  `http.NewCrossOriginProtection` (stdlib) refuses state-changing requests
  from other sites, so a web page the owner visits cannot post to it.
- **Logs go to stderr and desktop notifications are off** in `serve`, since the
  terminal app's log file and `osascript` make no sense in a pod.
- **The token is read once at startup** (`--token-file`, trimmed). A rotated
  Secret needs a pod restart, which the deployment handles (the 1Password
  operator's auto-restart annotation in the owner's cluster).
- **`contextcheck` is disabled** because a merge or re-run deliberately gets
  its own context: it must not be cancelled when the phone drops the
  connection or the process is asked to stop.

**Page mechanics.** `page.html` defines three templates: `page` (the document),
`main` (header, flash message, list) and `list`. The list polls `GET /prs`
every 10s and swaps itself. A `POST` answers with `main`, so the flash message
and the list update together. htmx is embedded (`static/htmx.min.js`, version
2.0.4) so the page loads nothing from other sites. The page is drawn from
`snapshot()`, a copy taken under the lock, so templates never touch the engine.

Three things to know when changing the page:

- **htmx hands `hx-target` and `hx-swap` down to child elements.** The list
  swaps itself (`hx-target="this"`), so every form inside it names `#main` as
  its own target. Without that a form's answer (header, message and list)
  lands where only the list was, and the header shows twice.
- **The flash message sits outside the list**, so the list's polling never
  replaces it. It goes away through a CSS animation (5s, 15s for an error).
- **Refresh waits for the fetch it asked for** (`Server.fetched`, a channel
  closed and replaced on every fetch result) and answers with the fresh list.

Only `POST` requests are logged (`logActions`), since the page polls every
10s. `static/favicon.svg` is the project logo: the favicon, the mark in the
page header and the image at the top of the README all use that one file.

**Shipping.** `Dockerfile` builds a distroless nonroot image and
`.github/workflows/image.yml` pushes it to `ghcr.io/utkuozdemir/gh-pr-lander`
for both amd64 and arm64 when a `v*` tag is pushed, and creates the GitHub
release for the tag. The repo is private, so the
package starts private too and has to be made public once in the package
settings. Deployment manifests live in the owner's homelab repo, not here.

## Merging: fast-forward through the refs API

The tool used to merge by commenting `/m`, which a bot turned into a
fast-forward. That bot was retired in Nov 2026. `m` and automerge now
call `Client.FastForwardMerge`: `PATCH /repos/{repo}/git/refs/heads/{base}` with
the PR's head SHA and `force: false`. GitHub moves the base branch only if that
is a fast-forward (422 `Update is not a fast forward` otherwise) and marks the
PR merged a moment later. No commit is created, so SHAs and signatures stay.

- **Branch protection is the real gate.** It applies to this ref update like to
  any push. The repositories this was built against set `enforce_admins:
  true` with no bypass allowances, so GitHub refuses an
  unapproved, red or behind PR even for admins. `StatusReady` only decides when
  to try. That is why there is no pre-merge re-check in the tool.
- **The SHA sent is the one from the fetch that was judged ready** (`p.HeadSHA`),
  never a fresh lookup, so a push landing in between is not merged unseen.
- **The call is synchronous**, so there is no comment counting, cooldown or
  duplicate check any more. A refused automerge sets `mergeFailed` for the PR
  and notifies. It is about one commit, so `checkTransitions` clears it (and the
  automerge check cooldown) when the head SHA changes. Losing the race to
  another merge is the common failure and is not a give-up at all: the client
  returns `ErrNotFastForward` for it, automerge stays on, and the next fetch
  shows the PR as behind. Only a 4xx answer (`ErrMergeRefused`) sets
  `mergeFailed`. A network or 5xx error is retried after the cooldown, since it
  says nothing about the commit. `markAutoCheck` still gates automerge, which keeps a second ref update
  from going out while the merged PR is still listed as open.
- **Right after a merge GitHub still lists the PR as open** for a moment, with
  the base branch tip equal to the PR's head. `behindBase` treats that as not
  behind, otherwise the `baseRefOid` comparison would call the merged PR
  behind and autorebase would rebase it.
- Writing refs on a private repo needs a classic token with the `repo` scope.

## Issue #1: spurious merge attempt after a re-run (the "rerun settling" guard)

**Symptom:** when a PR has failed checks and you press re-run, for a brief
moment the tool showed "all checks passed", which (with automerge on) fired a
wrong merge attempt (a `/m` comment back then) that gets rejected.

**Cause:** `RerunFailedJobs` resets the failed check runs. For a short window
GitHub's GraphQL `statusCheckRollup` drops the resetting checks from its
`contexts` list (and `totalCount`), so the only checks left are the
already-passing ones. `computeCIStatus` then sees all-passed → `StatusReady` →
automerge tries to merge. GitHub's rollup is transiently under-reporting, it is
not a bug in our computation per se.

**Fix:** `internal/engine/auto.go`, `applyRerunSettling`. When a re-run is
triggered (manual `checksRerunMsg` success, or autorerun `reran`) the PR is
marked "in flight" with the trigger time and the head SHA. While a PR is in
flight and within the settle grace, any transient `CIPassed`/`CINone` reading is
rewritten to `CIRunning` (and `OverallStatus` recomputed), keeping it out of
`StatusReady` so automerge can't fire. The guard clears as soon as GitHub
catches up (CI genuinely shows `CIRunning` again), the head SHA changes (a new
push), or the grace window elapses (`config.RerunSettleGrace`, derived from the
poll interval so it always spans at least one poll cycle). Note we deliberately
do **not** clear on `CIFailed`, because right after the trigger the PR still
shows the old failed state for a while before GitHub resets it to running — the
flicker happens after that.

If you ever revisit this, an alternative/complementary heuristic is to detect a
suspicious drop in total check count versus the previous fetch when the previous
fetch had failures. We chose the rerun-in-flight approach because it directly
matches the reported reproduction and is fully testable without extra API calls.
That count-drop heuristic is exactly what issue #7 (below) ended up needing.

## Issue #7: spurious "all passed" after an external rebase+push (the "checks settling" guard)

**Symptom:** you rebase a behind branch and force-push it *outside* the tool,
then refresh: the tool briefly says "1/1 passed, good to go", and only on the
next refresh shows the real "13/14 done".

**Cause:** the force-push creates a brand-new head commit, and GitHub does not
attach all the required checks to it atomically. For a few seconds the new
commit's `statusCheckRollup` reports a small, all-green subset (sometimes a
single passing check) before the rest of the workflows register. The tool
faithfully reports that snapshot → `CIPassed` → `StatusReady`. Same family as
issue #1 (rollup transiently under-reporting), but a different trigger, and the
issue #1 guard does **not** cover it: that guard only arms on an *in-tool*
action, and it explicitly *clears* on a head-SHA change — which is exactly what a
rebase+push is. So an external push is an unguarded window (and with automerge on
could fire a spurious merge attempt).

**Fix (two parts):**

1. **Honor the rollup's aggregate `state`** in `computeCIStatus`
   (`internal/github/status.go`). We already fetched `statusCheckRollup.state`
   but ignored it. When no fetched context failed/ran/pended but the aggregate
   state is `EXPECTED`/`PENDING` (and we saw at least one context), report
   `CIRunning` — GitHub itself is saying more checks are coming. Cheap and pure,
   but on its own it does *not* catch the "1 registered + green, state SUCCESS"
   instant, so it is only a secondary signal.

2. **Cross-fetch check-count settling** in `internal/engine/auto.go`,
   `applyChecksSettling` (keyed by PR via `checkBaseline`). For each PR we
   remember the head SHA, when we first saw it, the high-water check count on it,
   and the high-water count on the *previous* SHA. When the SHA just changed and
   the new commit's current check count is **below** the previous commit's
   count, the required checks have not all registered yet, so a transient
   `CIPassed`/`CINone` is rewritten to `CIRunning` (and `OverallStatus`
   recomputed). It relaxes once the count catches up (`prevMaxTotal <= current`)
   or the grace (`config.RerunSettleGrace`) elapses. A *first* sighting
   (`prevMaxTotal == 0`) is never suppressed, so a genuinely green PR shows ready
   immediately. This is the real fix for #7 because it needs no in-tool action to
   arm — it is the count-drop heuristic foreshadowed in the issue #1 notes.

**Re-runs started elsewhere.** The issue #1 guard arms only on a re-run this
process started. A re-run from GitHub's page, or from a second instance of the
tool (terminal app and server side by side), resets checks on the same commit
with nothing armed. `applyChecksSettling` covers it with the same count signal
on one commit: when the count falls below the commit's own high-water mark
(`CheckBaseline.DroppedAt`), a `CIPassed`/`CINone` reading is rewritten to
`CIRunning` until the count is back or the grace is over.

Both guards run each fetch (`applyChecksSettling(applyRerunSettling(...))`) and
only ever push toward `CIRunning`, so they compose without fighting. Tested in
`internal/engine/auto_test.go` (settling) and `internal/github/client_test.go`
(rollup state), no network or timing flakiness.

## Checks that are not required do not hold up a merge (`UNSTABLE`)

A PR whose required checks are all green can be merged while other checks are
still running, and GitHub's page says so ("Able to merge, some non-required
checks are still in progress"). The tool used to wait for every check, so a
check that never reports (a coverage service waiting for an upload, seen
hanging for 15+ minutes) kept a mergeable PR out of `StatusReady`, with
automerge waiting and "merge" refused.

The signal is `mergeStateStatus: UNSTABLE`, which GitHub computes for exactly
this: mergeable now, with a non-required check not passed. In
`computeCIStatus`, when checks are still running or pending and the state is
`UNSTABLE`, the result is `CIPassed` and `CheckSummary.Optional` holds how many
are still open, shown as "14/15 passed, 1 optional still running".

- **Per-check `isRequired` cannot be used.** It needs the PR number as an
  argument, and inside the search the tool runs (many PRs in one query) there
  is no way to give each node its own. Without the argument GitHub returns the
  contexts as null.
- **A failed check still means `CIFailed`**, required or not, even though
  GitHub would let the merge through. That stays the owner's call.
- **The settling guards still apply**, since they rewrite `CIPassed` back to
  `CIRunning` when a commit's check count is suspiciously low.
- `UNKNOWN` (GitHub has not computed the state yet) changes nothing, so the
  PR waits one more poll.
- The retired merge bot was stricter: it only merged on `clean` or
  `has_hooks`, so it also waited for non-required checks.

## Required checks that never started (`BLOCKED` but green)

A fork's workflows wait for a maintainer to approve them before they run. Until
then their required checks are not in `statusCheckRollup` at all: the rollup
lists only the checks that exist (all green) and its aggregate `state` is
`SUCCESS`. The GitHub UI shows them as "Expected, waiting for status to be
reported", but GraphQL has no field for missing required checks (`isRequired`
is only on contexts that exist). The only signal is `mergeStateStatus: BLOCKED`,
so `prFromNode` sets `MergeBlocked`, and `ComputeOverallStatus` turns a PR that
would otherwise be `StatusReady` into `StatusBlocked`. Automerge and manual `m`
both need `StatusReady`, so no merge is attempted (GitHub would refuse it). On a
live sample of approved PRs, every green one with a computed merge state was `CLEAN` or `BEHIND`, never `BLOCKED`, so this does not hold back
mergeable PRs. For the reason, `annotateWorkflowRuns` also fetches Actions runs
for such a PR and counts conclusion `action_required` into
`AwaitingApprovalRuns`, shown as "workflow awaiting approval". `w` approves
them (`POST .../actions/runs/{id}/approve`, through the same
`postToWorkflowRuns` helper as re-running failed jobs) and is listed in the
footer only on such a PR. It stays manual on purpose: approving lets a
contributor's code run in CI. Search results
often carry `mergeStateStatus: UNKNOWN` (GitHub computes it lazily), and that
never sets `MergeBlocked`.

## Failed check, but nothing to re-run yet

The GraphQL rollup and the Actions REST API disagree for a while after a job
fails. The rollup reports the failed check at once (`CIFailed`), but the
workflow run that owns it stays `in_progress` with no `conclusion` until every
other job in that run finishes (with long end-to-end jobs that is several minutes).
GitHub only lets you re-run failed jobs of a completed run, so in that window
there is genuinely nothing to re-run. `WorkflowSummary.Running` /
`pr.PR.RunningWorkflowRuns` carry the not-yet-completed run count so `t` can say
"workflow still running" instead of the misleading "no failed GitHub Actions
jobs". Autorerun needs nothing special: `shouldAutoRerun` already waits for
`FailedWorkflowRuns > 0`, which only becomes true once the run completes.

## Autorebase queue

With fast-forward merges, merging one PR makes every other PR on the same
base branch behind again, so rebasing them in parallel only starves CI runners.
`internal/tui/rebase.go` treats each repo + base branch as a queue ordered by
`db.yaml`'s `auto_rebase` map (PR key to an order number, appended on enable,
swapped by `K`/`J`). Everything else is derived from the current fetch:

- **Eligible** (`rebaseEligible`): not draft, no conflict, review not pending
  or changes requested, and CI not failed unless `autorerunWillHandle` (a flaky
  failure autorerun is about to re-run would otherwise hand the queue on, and
  both PRs would run CI). Only eligible PRs hold the queue, since rebasing a PR
  that cannot merge only burns CI.
- **In flight** (`inFlight`): not behind, being rebased (`model.rebasing`), or
  its last autorebase started from its current head (`autoRebaseState.sha`).
  `BaseUnknown` counts as not behind, so a PR GitHub is still computing holds
  the queue for a cycle.
- **Active** (`activeInQueue`): the first eligible PR in flight, so nothing
  happens, else the first eligible PR, which gets rebased.
- **One rebase at a time globally** (`model.rebasing`), not per queue: every
  rebase needs a YubiKey touch, and queues on different base branches of one
  repo share a clone. A manual `r` sets the same marker before its
  `tea.ExecProcess` runs (a poll result can be handled in between), and is
  refused while an autorebase runs.
- **No repeat on stale data**: the next fetch can still report the old commit
  as behind, so a PR whose last rebase started from its current head is in
  flight, not due. This holds even if the PR was reordered below another one
  meanwhile.
- **Failures are never retried automatically.** A failure is almost always a
  missed YubiKey touch, so the owner is away and a retry would fail the same
  way. The state keeps the SHA plus a `reason`, so the PR stays in flight and
  keeps its queue paused instead of handing it on (the next PR would fail too
  while nobody is there). Other queues go on. The row shows `failureReason`,
  matched on what git prints: `failed to write commit object` (signing, gpg
  and ssh alike), `CONFLICT`, `failed to push some refs`. `b` on a failed PR
  retries, and any new head commit (manual `r`, outside push) voids the
  failure since it is keyed by SHA. Real conflicts rarely get here, GitHub
  reports them as `DIRTY` and the PR is ineligible.
- **Reordering never preempts.** Moving a PR to the top changes who goes next.
  The in-flight PR keeps the queue until it merges or drops out.

The background run reuses `git.BuildRebaseScript` unchanged: countdown 0 and
stdin `/dev/null` (exec's default), so its "press Enter" `read` prompts hit EOF
and the script exits non-zero instead of hanging (verified in bash). Output is
captured and logged on failure. Signing prompts through the gpg program's own
GUI (pinentry, touch HUD), so no terminal is needed. The 10-minute timeout kills
only bash, so `cmd.WaitDelay` is what makes a hung git or ssh child (laptop
asleep mid-push) report back instead of holding `model.rebasing` forever.

## Testing

- Pure logic (`pr`, `store`) is tested directly.
- `github` is tested by injecting an `http.Client` whose `Transport` is a
  `roundTripFunc` returning canned responses — no real network. See
  `client_test.go`.
- The `Poller` is tested with a `fakeFetcher` (the `PRFetcher` interface).
- The engine is tested by constructing `State` values and calling its methods
  directly, with a `fakeClient`. The issue #1 and #7 regressions live in
  `internal/engine/auto_test.go`.
- The TUI is tested by constructing `model` values (with a `State` inside) and
  calling `Update` / action methods directly.
- `model.Update` returns `tea.Model`; cast back with `updated.(model)`.

## Build / test / lint

Tooling mirrors the other `utkuozdemir` repos (e.g. pv-migrate),
scaled down for a single binary. Use [Task](https://taskfile.dev):

```
task            # lint + test (default)
task fmt        # go mod tidy, go fix, golangci-lint --fix, golangci-lint fmt
task lint       # go mod tidy --diff + golangci-lint run
task test       # go test ./...
task test-race  # go test -race ./...   (the poller is concurrent — keep this clean)
task build      # go build -o gh-pr-lander .
task release    # tag the next version (svu) and push it, VERSION=vX.Y.Z to name one
```

Or the raw commands: `go build ./...`, `go test ./...`, `golangci-lint run ./...`.

- **Linters**: `.golangci.yml` is `default: all` with a curated disable list
  (config schema `version: "2"`). On top of the usual disables we
  also turn off the linters that fight Bubble Tea / GitHub-client idioms
  (`ireturn`, `recvcheck`, `varnamelen`, `cyclop`, `funlen`, `funcorder`,
  `tagliatelle`, `gosmopolitan`, `paralleltest`, `lll`) — each has a one-line
  reason in the file. `gosec` G204 is excluded because this tool shells out by
  design (git, gh, open, osascript). If you add code that trips one of these,
  prefer fixing it; only widen the disable list when the linter genuinely
  conflicts with an unavoidable idiom, and always leave a comment.
- **`exhaustruct_v5` noise locally**: golangci-lint 2.13+ reports it as
  `exhaustruct_v5`, which the `exhaustruct` disable does not cover, so a local
  run shows ~50 struct-literal hits that CI (pinned to v2.12.2) never sees. Use
  `golangci-lint run --new-from-rev=origin/main` to see only your own hits.
  That hides findings on lines the change did not touch, such as `unparam` on
  a function signature whose body changed. And the newer local version does
  not agree with CI on everything: 2.13 let repeated strings in test files
  pass that 2.12.2 rejects (`goconst`). So before pushing, run the version CI
  pins, which takes the guessing out:
  `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2 run ./...`
  Don't add `exhaustruct_v5` to the disable list while CI is on 2.12.x, an
  unknown linter name is a config error there.
- **Formatters**: gci, gofmt, gofumpt, goimports, golines (max-len 120), run via
  `golangci-lint fmt`. CI is `.github/workflows/build.yml` (tidy check, race
  tests, lint, build). A release is only a version tag: `task release` checks
  that HEAD is the tip of `origin/main` with a clean tree, picks the next
  version with svu from the commit types since the last tag, and pushes an
  annotated, unsigned tag, which starts `.github/workflows/image.yml`. That
  workflow pushes the image and then creates a GitHub release with generated
  notes. There are no binaries or goreleaser yet, on purpose.

Go version: see `go.mod` (currently 1.26.x). Uses generics-era stdlib helpers
(`min`/`max`, `strings.SplitSeq`) and Go 1.22+ loop semantics (no `x := x`
copies needed in closures).

### Gotcha: golines mangles raw strings inside long call expressions

`golines` (a formatter) once **corrupted the embedded bash rebase script**: when
a multi-line backtick raw string is an argument to a long `fmt.Sprintf(...)`
call, golines rewrites it into an escaped double-quoted string and dropped a
line (an `fi`), silently breaking the script. A plain `const x = ` + backtick
raw string (not inside a call) is left alone. So: **never put a multi-line raw
string literal directly inside a call expression that exceeds the line length.**
`internal/git/rebase.go` now builds the script from a `[]string` joined by
`"\n"`, which keeps every line short and formatter-proof, and
`rebase_test.go` asserts the `if`/`fi` balance to catch any future regression.

### Gotcha: shell-quote every value interpolated into the rebase script

Values from the GitHub API go into the bash script through `shellQuote`
(single quotes), never `%q`. Go's `%q` makes a double-quoted string, inside
which bash still runs `$(...)` and backticks, and Git allows both in branch
names. This matters since taken-over PRs are listed: their branch name comes
from a contributor. `TestBuildRebaseScriptKeepsBranchNamesLiteral` evaluates
the generated assignment in real bash to prove it stays literal.

## Conventions and gotchas

- **Never leak API shapes out of `internal/github`.** Map to `pr.PR` at the
  boundary (`prFromNode`).
- **Review threads** come from the same GraphQL search
  (`reviewThreads(first: 100)`). Threads past the first 100 are not fetched and
  count as unresolved, the same "unknown is not good" rule `computeCIStatus`
  uses for unfetched checks. The `Comments x/y` indicator is display only. It
  does not gate automerge.
- **Taken-over PRs** (a community PR you rebased, re-signed or added commits
  to) are listed with a `⇄ @author` tag. The search is `involves:@me`, which
  is only a cheap candidate set (it also matches PRs you merely commented on or
  were mentioned in), and `touchedBy` keeps a PR only if you opened it or
  authored or committed any of its last 100 commits (`allCommits` alias in
  `prQuery`). There is no search qualifier for "committed to", and scanning
  every open org PR is not viable (one org had ~340, ~8s per page, 502s).
  Blind spot: a PR you pushed to without ever commenting, being assigned or
  mentioned is never a candidate. Assigning yourself fixes that.
- **GraphQL cost is driven by the search page size.** GitHub charges by the
  worst-case node count (`first` of the search times every nested `first`), not
  by what comes back. With `search(first: 100)` the query cost 3, and 5 with
  threads and `allCommits`. `first: 25` brings the full query to 1. Pagination
  covers more than 25 PRs. Measure with `rateLimit { cost }` before adding
  nested connections.
- **`mergeStateStatus` only says `BEHIND` when the base branch enforces it**:
  "require up to date" on, plus at least one required check. Without that
  (seen on a repo with strict on but zero required checks) a behind PR
  reports `CLEAN`/`BLOCKED`/`UNSTABLE`. So `prFromNode` also compares
  `baseRefOid` (the commit the PR is based on) with the base branch tip, and
  marks the PR `BaseBehind` with `BehindUnenforced` set, shown as a blue
  "behind (not enforced)". Logic treats it exactly like `BaseBehind`, since
  the merge is a fast-forward. `baseRefOid` matched the PR's first commit's
  parent on every PR sampled, and costs nothing, while asking for that
  parent doubles the query cost. Where it is enforced, GitHub reports
  `BEHIND` even when a review is also missing, so the flag is honest. It
  stays off while `mergeStateStatus` is `UNKNOWN` (GitHub still computing),
  which says nothing about enforcement.
- **PR identity** is `pr.RepoShort()` → `"owner/repo#number"`. It is the key in
  every map (automerge, in-flight, recentlyMerged, cooldowns).
- **Branch deletion after merge** only happens on forks you own
  (`HeadRepoOwner == client.Username()`), never same-repo branches. See
  `detectMerged`.
- **`db.yaml` only stores enabled flags, the autorebase queue order and the
  last autorerun time.** Don't add derived state there, derive it from the API.
- **Nothing in this repo is signed**, even though the tool serves a
  signed-commit workflow. Commits are not GPG-signed (no YubiKey touch) and
  only carry the DCO signoff. The global git config signs by default, so commit
  with `git commit --no-gpg-sign -s`. The title is
  `type: imperative summary`, and the body is prose with each paragraph on a
  single line.
