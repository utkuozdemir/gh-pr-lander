# sidero-rebaser

Terminal tool for the Siderolabs PR rebase-and-merge workflow.

## The problem

At Siderolabs, every commit is GPG-signed with a YubiKey. We never click GitHub's merge button. All merges are fast-forward only, triggered by commenting `/m` on a PR after rebasing locally. The entire commit history stays signed by the team, no GitHub-authored commits.

This means a lot of rebasing. When someone merges before you, your PR goes stale. You stop what you're doing, switch branches, rebase, force push, wait for CI. Then you watch for CI to pass so you can comment `/m` before someone else merges and you go stale again. Flaky tests? Re-run, wait, hope nobody merges in the meantime. Multiply by the number of repos and open PRs.

## What this does

Keep it open in a terminal tab. It polls your open PRs across configured GitHub orgs and shows their status: base branch freshness, CI, reviews, and resolved inline comment threads (`Comments 4/5` in yellow while any are open, green once all are resolved, hidden when there are none).

PRs opened by someone else that you took over (rebased, re-signed or added commits to) are listed too, tagged `⇄ @author`. GitHub search can only find them if you commented on, were assigned to, or were mentioned in the PR, so assign yourself when you take one over.

A PR that looks approved and green but that GitHub still blocks from merging shows as `BLOCKED`, and automerge leaves it alone. The usual cause is a community PR whose workflows wait for a maintainer to approve them, shown as `⏸ workflow awaiting approval`. Press `w` to approve them and CI starts.

- `r` rebases. Drops into a real local `git rebase` so your YubiKey signs the commits. Returns to the TUI after a few seconds on success.
- `m` posts `/m` as a PR comment. The bot does the fast-forward merge.
- `a` toggles automerge. Posts `/m` automatically when the PR is ready. Gives up after 3 attempts (counted from actual `/m` comments on the PR).
- `A` toggles autorerun. Re-runs failed CI jobs automatically. Gives up based on workflow `run_attempt` from the GitHub API.
- `b` toggles autorebase. Rebases the PR in the background when it falls behind its base branch, one PR at a time per repo and base branch (see below). A failed rebase, usually a missed YubiKey touch, is not retried: the row shows why it failed, a notification asks for you, and its queue waits until you press `b` on it again to retry, or rebase it with `r`.
- `t` re-runs failed CI jobs manually.
- `w` approves the workflows of a community PR that wait for a maintainer, like GitHub's "Approve workflows to run" button. It shows in the footer only on such a PR.
- `Enter` opens the PR in your browser.

### Autorebase queue

With fast-forward merges, merging one PR makes every other PR on the same base branch behind again, so rebasing several at once only makes them wait for CI runners for nothing. Autorebase therefore treats each repo and base branch as a queue, in the order you turned it on. The first PR that could merge once it is up to date (not a draft, no conflicts, review not pending, CI not failed, or failed but autorerun is re-running it) is the active one, shown as `▶ 1/3` in orange. While it is up to date and in CI, the others wait (`⏳ 2/3`). Once it merges or drops out, the next one is rebased. `K` and `J` move the selected PR up and down its queue, which changes who goes next but never interrupts the active PR. To let a hotfix jump the queue, move it to the top and turn autorebase off on the active PR.

Autorebase runs the same script as `r`, without taking over the terminal. Signing still needs your YubiKey touch, so it only gets far while you are at the machine. In repos that do not require branches to be up to date, a PR that is behind shows as `behind (not enforced)` in blue, and autorebase treats it like any other behind PR. Combine it with automerge for the full loop: rebase, CI, `/m`, then the next PR.

Merged PRs stay visible for an hour with a purple badge, so you can see what went through. The head branch on your fork is automatically deleted after a confirmed merge.

Desktop notifications use a different macOS sound per event, so you can tell them apart without looking:

| Event | Sound |
|---|---|
| Ready to merge | Glass |
| Merge posted by automerge | Pop |
| Merged | Hero |
| Closed without merge | Bottle |
| Checks failed | Basso |
| PR needs rebase | Submarine |
| Autorebase needs you | Sosumi |
| Automerge gave up | Funk |
| Autorerun gave up | Frog |

When automation is on for a PR, the event it is about to handle stays quiet and only its outcome notifies: no "ready to merge" with automerge on, no "checks failed" while autorerun can re-run the failure (a GitHub Actions job), no "needs rebase" with autorebase on.

The tool keeps its own repo clones under `~/.sidero-rebaser/repos/` and never touches your working directories.

## Setup

Needs a GitHub token. Any of these:
- `GITHUB_TOKEN` or `GH_TOKEN` environment variable
- `gh` CLI logged in (`gh auth login`)

```
go install github.com/utkuozdemir/sidero-rebaser@latest
sidero-rebaser
```

Or `go run .` from source. All settings have defaults, `--help` for the full list:

```
--orgs                      comma-separated GitHub orgs (default: siderolabs,cosi-project)
--interval                  poll interval when idle (default: 2m, minimum: 10s); polls faster automatically while CI is running
--data-dir                  data directory (default: ~/.sidero-rebaser)
--merge-comment             comment to post when merging (default: /m)
--auto-max-retries          max auto attempts before giving up (default: 3)
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
| `m` | Post merge comment (ff merge via bot) |
| `a` | Toggle automerge for selected PR |
| `A` | Toggle autorerun for selected PR |
| `b` | Toggle autorebase for selected PR |
| `K` / `J` | Move selected PR up / down its autorebase queue |
| `t` | Re-run failed CI jobs |
| `Enter` / `o` | Open in browser |
| `R` | Refresh now |
| `j/k` / arrows | Navigate |
| `q` | Quit |

## Git identity and signing

Clones under `~/.sidero-rebaser/repos/` inherit your global gitconfig. If you need different settings (signing key, email, etc.), put them in `~/.config/sidero-rebaser/gitconfig`:

```
[user]
    name = Utku Ozdemir
    email = utku.ozdemir@siderolabs.com
    signingkey = 11177A43C6E3752E682AC690DBD13117B0A14E93
[commit]
    gpgSign = true
[tag]
    gpgSign = true
```

Gets included in every clone automatically.

## Logs

`~/.sidero-rebaser/sidero-rebaser.log`
