# CLAUDE.md

You (Claude) work on this repository. **Part of your job, every time, is to keep
this file accurate and richer than you found it.** When you learn something
non-obvious about the code, GitHub's API behavior, a design decision, a gotcha,
or a user preference, add it here. Prefer updating an existing section over
appending a new one. Treat this file as your long-term memory for the project:
the better it is, the faster and safer every future change becomes. Keep it
honest — if something here turns out to be wrong, fix it.

## What this tool is

`sidero-rebaser` is a terminal (Bubble Tea) tool for the Siderolabs PR
workflow. At Siderolabs every commit is GPG-signed with a YubiKey, the GitHub
merge button is never used, and merges happen by commenting `/m` on a PR after
the author has rebased locally onto the latest base. That means constant
rebasing, watching CI, and racing to comment `/m` before someone else merges and
your PR goes stale. This tool watches all your open PRs across configured orgs
and gives you one-key rebase / merge-comment / rerun, plus automerge and
autorerun automation.

User-facing details live in `README.md`. This file is about *how the code
works* and *why*.

## Architecture

The code is split into small packages under `internal/`. The dependency
direction is strict and acyclic:

```
main  ──>  tui  ──>  github ──> pr
                ├──>  git    ──> pr
                ├──>  store
                ├──>  notify
                └──>  config
        github ──> pr,  config-independent
```

- **`internal/pr`** — the domain model. `PR` struct, the status enums
  (`BaseStatus`, `CIStatus`, `ReviewStatus`, `OverallStatus`), `CheckSummary`,
  `ComputeOverallStatus`, and `Sort`. Pure, deterministic, no I/O, no API
  shapes. This is the lingua franca every other package speaks.
- **`internal/github`** — the *only* package that talks to the GitHub API
  (GraphQL for PR search, REST for comments / Actions / branch deletion). It
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
  bell). Enable/disable is process-global (`atomic.Bool`).
- **`internal/config`** — the resolved `Config` value, built once in `main`.
- **`internal/tui`** — Bubble Tea model, update loop, views, styles, and the
  auto-merge / auto-rerun logic. Split across `model.go` (model + Update +
  actions), `auto.go` (automation + the rerun-settling guard), `merged.go`
  (merged/closed tracking + transition notifications), `view.go`, `styles.go`,
  `commands.go` (messages + `tea.Cmd`s), `client.go` (the `Client` interface).
- **`main.go`** — thin wiring only: parse flags, build the client and poller,
  run the program and the poller together under an errgroup.

### Interfaces live on the consumer side

The TUI does not depend on `*github.Client` directly. It declares a narrow
`Client` interface (`internal/tui/client.go`) listing only the methods it uses;
`*github.Client` satisfies it structurally. Same pattern for the poller:
`github.PRFetcher` is the one-method slice the poller needs. This is what makes
the TUI and poller unit-testable with fakes and no network. **When you add a
new GitHub call used by the TUI, add it to both `*github.Client` and the `tui.Client`
interface.**

### Responsiveness: the background Poller (issue #2)

Polling used to be UI-driven: a `tickMsg` every interval kicked off a fetch, and
effects of an action only showed up on the next tick. Now `github.Poller` runs
in its own goroutine (`Poller.Run(ctx)`), fetches immediately on start, then on
every interval tick **and** on every `Refresh()` request, and streams
`FetchResult`s over a channel. The TUI consumes them with `waitForResultsCmd`,
which blocks on the channel and re-issues itself after each result — a
continuous update stream.

After any action (post `/m`, rerun, rebase, toggle automerge) the model calls
`refresh()` (wired to `Poller.Refresh`) so the new state is fetched right away
instead of waiting for the interval. `Refresh()` is non-blocking and coalesces
(buffered channel of size 1). Short follow-up refreshes at 15s/30s
(`delayedRefreshCmd`) cover GitHub's own propagation lag (e.g. the `/m` bot
taking a moment to merge).

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

## Issue #1: spurious `/m` after a re-run (the "rerun settling" guard)

**Symptom:** when a PR has failed checks and you press re-run, for a brief
moment the tool showed "all checks passed", which (with automerge on) fired a
wrong `/m` that the bot rejects.

**Cause:** `RerunFailedJobs` resets the failed check runs. For a short window
GitHub's GraphQL `statusCheckRollup` drops the resetting checks from its
`contexts` list (and `totalCount`), so the only checks left are the
already-passing ones. `computeCIStatus` then sees all-passed → `StatusReady` →
automerge posts `/m`. GitHub's rollup is transiently under-reporting; it is not
a bug in our computation per se.

**Fix:** `internal/tui/auto.go`, `applyRerunSettling`. When a re-run is
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
could fire a spurious `/m`).

**Fix (two parts):**

1. **Honor the rollup's aggregate `state`** in `computeCIStatus`
   (`internal/github/status.go`). We already fetched `statusCheckRollup.state`
   but ignored it. When no fetched context failed/ran/pended but the aggregate
   state is `EXPECTED`/`PENDING` (and we saw at least one context), report
   `CIRunning` — GitHub itself is saying more checks are coming. Cheap and pure,
   but on its own it does *not* catch the "1 registered + green, state SUCCESS"
   instant, so it is only a secondary signal.

2. **Cross-fetch check-count settling** in `internal/tui/auto.go`,
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

Both guards run each fetch (`applyChecksSettling(applyRerunSettling(...))`) and
only ever push toward `CIRunning`, so they compose without fighting. Tested in
`internal/tui/auto_test.go` (settling) and `internal/github/client_test.go`
(rollup state), no network or timing flakiness.

## Testing

- Pure logic (`pr`, `store`) is tested directly.
- `github` is tested by injecting an `http.Client` whose `Transport` is a
  `roundTripFunc` returning canned responses — no real network. See
  `client_test.go`.
- The `Poller` is tested with a `fakeFetcher` (the `PRFetcher` interface).
- The TUI is tested by constructing `model` values and calling `Update` /
  action methods / `applyRerunSettling` directly, with a `fakeClient`
  implementing `tui.Client`. The issue #1 regression lives in
  `internal/tui/auto_test.go`.
- `model.Update` returns `tea.Model`; cast back with `updated.(model)`.

## Build / test / lint

Tooling mirrors the other `utkuozdemir`/Siderolabs repos (e.g. pv-migrate),
scaled down for a single binary. Use [Task](https://taskfile.dev):

```
task            # lint + test (default)
task fmt        # go mod tidy, go fix, golangci-lint --fix, golangci-lint fmt
task lint       # go mod tidy --diff + golangci-lint run
task test       # go test ./...
task test-race  # go test -race ./...   (the poller is concurrent — keep this clean)
task build      # go build -o sidero-rebaser .
```

Or the raw commands: `go build ./...`, `go test ./...`, `golangci-lint run ./...`.

- **Linters**: `.golangci.yml` is `default: all` with a curated disable list
  (config schema `version: "2"`). On top of the standard Siderolabs disables we
  also turn off the linters that fight Bubble Tea / GitHub-client idioms
  (`ireturn`, `recvcheck`, `varnamelen`, `cyclop`, `funlen`, `funcorder`,
  `tagliatelle`, `gosmopolitan`, `paralleltest`, `lll`) — each has a one-line
  reason in the file. `gosec` G204 is excluded because this tool shells out by
  design (git, gh, open, osascript). If you add code that trips one of these,
  prefer fixing it; only widen the disable list when the linter genuinely
  conflicts with an unavoidable idiom, and always leave a comment.
- **Formatters**: gci, gofmt, gofumpt, goimports, golines (max-len 120), run via
  `golangci-lint fmt`. CI is `.github/workflows/build.yml` (tidy check, race
  tests, lint, build) — no release/goreleaser flow on purpose.

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

## Conventions and gotchas

- **Never leak API shapes out of `internal/github`.** Map to `pr.PR` at the
  boundary (`prFromNode`).
- **PR identity** is `pr.RepoShort()` → `"owner/repo#number"`. It is the key in
  every map (automerge, in-flight, recentlyMerged, cooldowns).
- **Branch deletion after merge** only happens on forks you own
  (`HeadRepoOwner == client.Username()`), never same-repo branches. See
  `detectMerged`.
- **`db.yaml` only stores enabled flags + last autorerun time.** Don't add
  derived state there; derive it from the API.
- Commit style for this repo follows the Siderolabs format (see the user's
  global instructions): `type: imperative summary`, prose body as single-line
  paragraphs, `Signed-off-by`, no Claude co-author.
