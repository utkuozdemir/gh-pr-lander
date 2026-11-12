<p align="center"><img src="internal/server/static/favicon.svg" width="96" alt="gh-pr-lander logo"></p>

# gh-pr-lander

Lands your GitHub pull requests in repositories where commits are signed and merges are fast-forward only: a terminal app, and a server with a small page for when you are away from it.

## The problem

Some teams sign every commit with a hardware key such as a YubiKey and never click GitHub's merge button. All merges are fast-forward only: after rebasing locally, the base branch is moved to the PR's head commit through the GitHub API. The entire commit history stays signed by the team, no GitHub-authored commits.

This means a lot of rebasing. When someone merges before you, your PR goes stale. You stop what you're doing, switch branches, rebase, force push, wait for CI. Then you watch for CI to pass so you can merge before someone else merges and you go stale again. Flaky tests? Re-run, wait, hope nobody merges in the meantime. Multiply by the number of repos and open PRs.

## What this does

Keep it open in a terminal tab. It polls your open PRs across configured GitHub orgs and shows their status: base branch freshness, CI, reviews, and resolved inline comment threads (`Comments 4/5` in yellow while any are open, green once all are resolved, hidden when there are none).

PRs opened by someone else that you took over (rebased, re-signed or added commits to) are listed too, tagged `⇄ @author`. GitHub search can only find them if you commented on, were assigned to, or were mentioned in the PR, so assign yourself when you take one over.

A PR that looks approved and green but that GitHub still blocks from merging shows as `BLOCKED`, and automerge leaves it alone. The usual cause is a community PR whose workflows wait for a maintainer to approve them, shown as `⏸ workflow awaiting approval`. Press `w` to approve them and CI starts.

- `r` rebases. Drops into a real local `git rebase` so your YubiKey signs the commits. Returns to the TUI after a few seconds on success.
- `m` fast-forwards the base branch to the PR's head commit. Branch protection still applies, so GitHub refuses a PR that is unapproved, red or behind.
- Next to the checks, each PR shows how often they were re-run for the current commit (`2 re-runs`, or `re-run 2 of 3` with autorerun on).
- `a` toggles automerge. Merges automatically when the PR is ready: approved, up to date, and every required check green. A check that is not required may still be running. You can make automerge on a PR wait for more: `2` or `3` asks for that many approvals (`1` goes back to whatever the repository requires), and `c` makes it wait until every review comment is resolved. A comment of your own that nobody replied to is a note for the reviewers. It is shown next to the count (`1/1 resolved · 1 note`) and automerge never waits for it. Set them before turning automerge on, since automerge merges a ready PR at once. These conditions hold back automerge only, `m` still merges whenever GitHub would accept it, and they stay with the PR until it is merged or closed. If GitHub refuses the merge, it tries again only after the PR gets a new head commit. A network or server error is retried after the cooldown.
- `A` toggles autorerun. Re-runs failed CI jobs automatically. Gives up based on workflow `run_attempt` from the GitHub API.
- `b` toggles autorebase. Rebases the PR in the background when it falls behind its base branch, one PR at a time per repo and base branch (see below). A failed rebase, usually a missed YubiKey touch, is not retried: the row shows why it failed, a notification asks for you, and its queue waits until you press `b` on it again to retry, or rebase it with `r`.
- `t` re-runs failed CI jobs manually.
- `w` approves the workflows of a community PR that wait for a maintainer, like GitHub's "Approve workflows to run" button. It shows in the footer only on such a PR.
- `Enter` opens the PR in your browser.

### Autorebase queue

With fast-forward merges, merging one PR makes every other PR on the same base branch behind again, so rebasing several at once only makes them wait for CI runners for nothing. Autorebase therefore treats each repo and base branch as a queue, in the order you turned it on. The first PR that could merge once it is up to date (not a draft, no conflicts, review not pending, CI not failed, or failed but autorerun is re-running it) is the active one, shown as `▶ 1/3` in orange. While it is up to date and in CI, the others wait (`⏳ 2/3`). Once it merges or drops out, the next one is rebased. `K` and `J` move the selected PR up and down its queue, which changes who goes next but never interrupts the active PR. To let a hotfix jump the queue, move it to the top and turn autorebase off on the active PR.

Autorebase runs the same script as `r`, without taking over the terminal. Signing still needs your YubiKey touch, so it only gets far while you are at the machine. In repos that do not require branches to be up to date, a PR that is behind shows as `behind (not enforced)` in blue, and autorebase treats it like any other behind PR. Combine it with automerge for the full loop: rebase, CI, merge, then the next PR.

Merged PRs stay visible for an hour with a purple badge, so you can see what went through. The head branch on your fork is automatically deleted after a confirmed merge.

Desktop notifications use a different macOS sound per event, so you can tell them apart without looking:

| Event | Sound |
|---|---|
| Ready to merge | Glass |
| Merged | Hero |
| Closed without merge | Bottle |
| Checks failed | Basso |
| PR needs rebase | Submarine |
| Autorebase needs you | Sosumi |
| Automerge gave up | Funk |
| Autorerun gave up | Frog |

When automation is on for a PR, the event it is about to handle stays quiet and only its outcome notifies: no "ready to merge" with automerge on, no "checks failed" while autorerun can re-run the failure (a GitHub Actions job), no "needs rebase" with autorebase on.

The tool keeps its own repo clones under `~/.gh-pr-lander/repos/` and never touches your working directories.

## Setup

Needs a GitHub token. Any of these:
- `GITHUB_TOKEN` or `GH_TOKEN` environment variable
- `gh` CLI logged in (`gh auth login`)

```
go install github.com/utkuozdemir/gh-pr-lander@latest
gh-pr-lander
```

`gh-pr-lander version` prints which build it is. The terminal app shows the same in its header, and the server at the bottom of its page.

Or `go run .` from source. All settings have defaults, `--help` for the full list:

```
--orgs                      comma-separated GitHub orgs to watch (default: every PR of yours on GitHub)
--interval                  poll interval when idle (default: 2m, minimum: 10s); polls faster automatically while CI is running
--data-dir                  data directory (default: ~/.gh-pr-lander)
--auto-max-retries          max autorerun attempts before giving up (default: 3)
--auto-cooldown             cooldown between auto-merge/rerun attempts (default: 5m)
--notifications             desktop notifications (default: true)
--rebase-countdown          seconds to show success before returning (default: 3)
--merged-display-duration   how long to show merged PRs (default: 1h)
--delete-merged-branches    delete branch on fork after merge (default: true)
```

## Key bindings

| Key | Action |
|---|---|
| `r` | Rebase (local git, YubiKey signing) |
| `m` | Fast-forward merge |
| `a` | Toggle automerge for selected PR |
| `1` / `2` / `3` | Approvals automerge waits for (`1` is the repository's own rule) |
| `c` | Make automerge wait until every review comment is resolved |
| `A` | Toggle autorerun for selected PR |
| `b` | Toggle autorebase for selected PR |
| `K` / `J` | Move selected PR up / down its autorebase queue |
| `t` | Re-run failed CI jobs |
| `Enter` / `o` | Open in browser |
| `R` | Refresh now |
| `j/k` / arrows | Navigate |
| `?` | Explain every key and status |
| `q` | Quit |

## Server

The terminal app only works while it runs, and merging needs no YubiKey. `gh-pr-lander serve` is the same tool with no terminal: an always-on process that polls your PRs and serves a small page, so you can ask for a merge from a phone or a browser while the laptop is closed. Nothing is written on the PR itself.

```
gh-pr-lander serve --listen :8080 --token-file /path/to/token --data-dir /data
```

The page lists your PRs with their status. Per PR you can switch automerge and autorerun, merge now, and re-run failed jobs. Next to the Automerge switch, in the same box, are the two things it can wait for: `Approvals` (default, 2 or 3) and `Comments` (any or resolved). Set them before turning automerge on. When a condition is holding automerge back, the row says what it waits for. The server does those, and deletes your fork branch after a merge. It never rebases, since that needs the YubiKey. A PR that is behind waits for the terminal app.

- **Token:** `--token-file` names a file holding a GitHub token that can write to the repositories (a classic token with the `repo` scope works). Without the flag it falls back to `GITHUB_TOKEN`, `GH_TOKEN` or `gh`, which is handy for trying it locally.
- **State:** the switches are kept in `db.yaml` under `--data-dir` (default `~/.gh-pr-lander/server`). Give it a volume that survives restarts.
- **No login yet:** anything that can reach the page can use it, so serve it only on a network you trust, such as a home LAN or a private VPN. Requests coming from other websites are refused. Branch protection still decides whether a merge is accepted.
- **Only your own PRs:** a PR you took over from someone else is left to the terminal app, since its author can still push to it.
- **Side by side with the terminal app:** both work on their own and keep their own switches. If both have automerge on for one PR, the second merge attempt changes nothing.

Hover over any button or status on the page for a one-line explanation of what it does or means. In the terminal app, `?` shows the same explanations for every key and status.

`GET /healthz` answers a short `ok` for liveness and readiness probes. Point probes there and not at the page, which is larger than the 10KB a Kubernetes probe reads.

A container image is published as `ghcr.io/utkuozdemir/gh-pr-lander` for each version tag. In a deployment, name the organizations to watch with `--orgs`. Its entrypoint is the binary, so pass `serve` and the flags as arguments.

## Git identity and signing

The clones under `~/.gh-pr-lander/repos/` use your global git config, so rebases are signed like any other commit of yours.

If these clones need a different identity or signing key, git can do that itself. Add this to your global git config and put the settings into the file it points at:

```
[includeIf "gitdir:~/.gh-pr-lander/repos/"]
    path = ~/.config/git/pr-lander
```

With a custom `--data-dir`, use that directory in the `gitdir:` pattern.

## Logs

`~/.gh-pr-lander/gh-pr-lander.log`
