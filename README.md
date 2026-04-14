# sidero-rebaser

Terminal tool for the Siderolabs PR rebase-and-merge workflow.

## The problem

At Siderolabs, every commit is GPG-signed with a YubiKey. We never click GitHub's merge button. All merges are fast-forward only, triggered by commenting `/m` on a PR after rebasing locally. The entire commit history stays signed by the team, no GitHub-authored commits.

This means a lot of rebasing. When someone merges before you, your PR goes stale. You stop what you're doing, switch branches, rebase, force push, wait for CI. Then you watch for CI to pass so you can comment `/m` before someone else merges and you go stale again. Flaky tests? Re-run, wait, hope nobody merges in the meantime. Multiply by the number of repos and open PRs.

## What this does

Keep it open in a terminal tab. It polls your open PRs across configured GitHub orgs and shows their status: base branch freshness, CI, reviews.

- `r` rebases. Drops into a real local `git rebase` so your YubiKey signs the commits. Returns to the TUI after a few seconds on success.
- `m` posts `/m` as a PR comment. The bot does the fast-forward merge.
- `a` toggles automerge. Posts `/m` automatically when the PR is ready. Gives up after 3 attempts (counted from actual `/m` comments on the PR).
- `A` toggles autorerun. Re-runs failed CI jobs automatically. Gives up based on workflow `run_attempt` from the GitHub API.
- `t` re-runs failed CI jobs manually.
- `Enter` opens the PR in your browser.

Merged PRs stay visible for an hour with a purple badge, so you can see what went through. The head branch on your fork is automatically deleted after a confirmed merge.

Desktop notifications fire when a PR becomes mergeable, goes stale, or has failing checks.

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
--interval                  poll interval (default: 2m, minimum: 10s)
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
