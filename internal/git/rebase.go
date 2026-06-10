// Package git builds the shell script that performs a local, signed rebase.
//
// The rebase intentionally runs as a real foreground process (not via the Go
// git libraries) because every Siderolabs commit is GPG-signed with a YubiKey,
// which requires an interactive touch/PIN prompt. The TUI shells out to this
// script with tea.ExecProcess so the user sees git output and can interact
// with the YubiKey.
package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/utkuozdemir/sidero-rebaser/internal/pr"
)

// ConfigPath returns the path to the optional user-provided gitconfig that is
// included into every clone (signing key, identity, etc.).
func ConfigPath() string {
	home, _ := os.UserHomeDir()

	return filepath.Join(home, ".config", "sidero-rebaser", "gitconfig")
}

// RepoDir returns the path where the tool keeps its own clone of a fork, e.g.
// ~/.sidero-rebaser/repos/utkuozdemir/cosi-runtime. The tool never touches the
// user's own working directories.
func RepoDir(dataDir, forkOwner, forkName string) string {
	return filepath.Join(dataDir, "repos", forkOwner, forkName)
}

// BuildRebaseScript creates a bash script that:
//  1. Ensures the repo is cloned into the tool's own data directory
//  2. Sets up remotes (origin = fork, upstream = org repo)
//  3. Fetches latest
//  4. Checks out the PR branch
//  5. Rebases onto upstream/base
//  6. Force pushes to origin (fork)
func BuildRebaseScript(dataDir string, p pr.PR, countdown int) (string, error) {
	if p.HeadRepoOwner == "" || p.HeadRepoName == "" {
		return "", errors.New("cannot rebase: PR head repository unknown (fork may have been deleted)")
	}

	repoPath := RepoDir(dataDir, p.HeadRepoOwner, p.HeadRepoName)
	forkURL := fmt.Sprintf("git@github.com:%s/%s.git", p.HeadRepoOwner, p.HeadRepoName)
	upstreamURL := fmt.Sprintf("git@github.com:%s/%s.git", p.RepoOwner, p.RepoName)
	isFork := p.HeadRepoOwner != p.RepoOwner

	rebaseTarget := "origin/" + p.BaseBranch
	if isFork {
		rebaseTarget = "upstream/" + p.BaseBranch
	}

	// Ensure parent directory exists (for first clone).
	parentDir := filepath.Dir(repoPath)
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		return "", fmt.Errorf("create directory %s: %w", parentDir, err)
	}

	// The script is built line by line (rather than one big literal) so each
	// line stays short and formatters never reflow the embedded bash. The
	// foreground process lets the user read git output and answer YubiKey
	// prompts.
	blankEcho := `echo ""`
	lines := []string{
		"#!/bin/bash",
		"",
		"# Pause on any error so the user can read what went wrong",
		"pause_on_error() {",
		`    echo ""`,
		`    echo "!!! Failed (exit code $1)"`,
		`    echo ""`,
		`    echo "Press Enter to return to sidero-rebaser..."`,
		"    read -r",
		`    exit "$1"`,
		"}",
		"trap 'pause_on_error $?' ERR",
		"set -eE",
		"",
		fmt.Sprintf("REPO_PATH=%q", repoPath),
		fmt.Sprintf("FORK_URL=%q", forkURL),
		fmt.Sprintf("UPSTREAM_URL=%q", upstreamURL),
		fmt.Sprintf("PR_BRANCH=%q", p.HeadBranch),
		fmt.Sprintf("BASE_BRANCH=%q", p.BaseBranch),
		fmt.Sprintf("IS_FORK=%t", isFork),
		fmt.Sprintf("REBASE_TARGET=%q", rebaseTarget),
		"",
		blankEcho,
		`echo "==> Rebasing ${PR_BRANCH} onto ${REBASE_TARGET}"`,
		blankEcho,
		"",
		"# Clone if this is the first time we see this repo",
		`if [ ! -d "${REPO_PATH}/.git" ]; then`,
		`    echo "--- Cloning ${FORK_URL} (first time setup)..."`,
		`    git clone "${FORK_URL}" "${REPO_PATH}"`,
		"fi",
		"",
		`cd "${REPO_PATH}"`,
		"",
		"# Include user-provided gitconfig (signing keys, user identity, etc.)",
		"# Git silently ignores the include if the file doesn't exist.",
		fmt.Sprintf("git config --local include.path %q", ConfigPath()),
		"",
		"# Ensure origin points to fork",
		`git remote set-url origin "${FORK_URL}"`,
		"",
		"# Set up upstream remote for fork-based PRs",
		`if [ "${IS_FORK}" = "true" ]; then`,
		"    if git remote get-url upstream >/dev/null 2>&1; then",
		`        git remote set-url upstream "${UPSTREAM_URL}"`,
		"    else",
		`        git remote add upstream "${UPSTREAM_URL}"`,
		"    fi",
		"fi",
		"",
		`echo "--- Fetching origin..."`,
		"git fetch origin --prune",
		"",
		`if [ "${IS_FORK}" = "true" ]; then`,
		`    echo "--- Fetching upstream..."`,
		"    git fetch upstream --prune",
		"fi",
		"",
		`echo "--- Checking out ${PR_BRANCH}..."`,
		`git checkout -B "${PR_BRANCH}" "origin/${PR_BRANCH}"`,
		"",
		`echo "--- Rebasing onto ${REBASE_TARGET}..."`,
		"# Rebase needs special handling — abort on failure instead of leaving a dirty state",
		`if ! git rebase "${REBASE_TARGET}"; then`,
		`    echo ""`,
		`    echo "!!! Rebase had conflicts — aborting."`,
		"    git rebase --abort 2>/dev/null || true",
		`    echo ""`,
		`    echo "Press Enter to return to sidero-rebaser..."`,
		"    read -r",
		"    exit 1",
		"fi",
		"",
		`echo "--- Force pushing to origin/${PR_BRANCH}..."`,
		`git push --force-with-lease origin "${PR_BRANCH}"`,
		"",
		blankEcho,
		`echo "==> Rebase complete!"`,
		blankEcho,
		fmt.Sprintf("for i in $(seq %d -1 1); do", countdown),
		`    printf "\rReturning in %d..." "$i"`,
		"    sleep 1",
		"done",
		blankEcho,
		"",
	}

	return strings.Join(lines, "\n"), nil
}
