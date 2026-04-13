package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// gitconfigPath returns the path to the optional user-provided gitconfig.
func gitconfigPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "sidero-rebaser", "gitconfig")
}

// repoDir returns the path where the tool keeps its own clone of a fork.
// Uses the fork's owner/name, e.g. ~/.sidero-rebaser/repos/utkuozdemir/cosi-runtime
func repoDir(dataDir, forkOwner, forkName string) string {
	return filepath.Join(dataDir, "repos", forkOwner, forkName)
}

// buildRebaseScript creates a bash script that:
// 1. Ensures the repo is cloned into the tool's own data directory
// 2. Sets up remotes (origin = fork, upstream = org repo)
// 3. Fetches latest
// 4. Checks out the PR branch
// 5. Rebases onto upstream/base
// 6. Force pushes to origin (fork)
//
// The script runs in the foreground so the user sees output and can
// interact with YubiKey prompts during signing.
func buildRebaseScript(dataDir string, pr PR, countdown int) (string, error) {
	if pr.HeadRepoOwner == "" || pr.HeadRepoName == "" {
		return "", fmt.Errorf("cannot rebase: PR head repository unknown (fork may have been deleted)")
	}

	repoPath := repoDir(dataDir, pr.HeadRepoOwner, pr.HeadRepoName)
	forkURL := fmt.Sprintf("git@github.com:%s/%s.git", pr.HeadRepoOwner, pr.HeadRepoName)
	upstreamURL := fmt.Sprintf("git@github.com:%s/%s.git", pr.RepoOwner, pr.RepoName)
	isFork := pr.HeadRepoOwner != pr.RepoOwner

	rebaseTarget := "origin/" + pr.BaseBranch
	if isFork {
		rebaseTarget = "upstream/" + pr.BaseBranch
	}

	// Ensure parent directory exists (for first clone)
	parentDir := filepath.Dir(repoPath)
	if err := os.MkdirAll(parentDir, 0o755); err != nil {
		return "", fmt.Errorf("create directory %s: %w", parentDir, err)
	}

	return fmt.Sprintf(`#!/bin/bash

# Pause on any error so the user can read what went wrong
pause_on_error() {
    echo ""
    echo "!!! Failed (exit code $1)"
    echo ""
    echo "Press Enter to return to sidero-rebaser..."
    read -r
    exit "$1"
}
trap 'pause_on_error $?' ERR
set -eE

REPO_PATH=%[1]q
FORK_URL=%[2]q
UPSTREAM_URL=%[3]q
PR_BRANCH=%[4]q
BASE_BRANCH=%[5]q
IS_FORK=%[6]t
REBASE_TARGET=%[7]q

echo ""
echo "==> Rebasing ${PR_BRANCH} onto ${REBASE_TARGET}"
echo ""

# Clone if this is the first time we see this repo
if [ ! -d "${REPO_PATH}/.git" ]; then
    echo "--- Cloning ${FORK_URL} (first time setup)..."
    git clone "${FORK_URL}" "${REPO_PATH}"
fi

cd "${REPO_PATH}"

# Include user-provided gitconfig (signing keys, user identity, etc.)
# Git silently ignores the include if the file doesn't exist.
git config --local include.path %[8]q

# Ensure origin points to fork
git remote set-url origin "${FORK_URL}"

# Set up upstream remote for fork-based PRs
if [ "${IS_FORK}" = "true" ]; then
    if git remote get-url upstream >/dev/null 2>&1; then
        git remote set-url upstream "${UPSTREAM_URL}"
    else
        git remote add upstream "${UPSTREAM_URL}"
    fi
fi

echo "--- Fetching origin..."
git fetch origin --prune

if [ "${IS_FORK}" = "true" ]; then
    echo "--- Fetching upstream..."
    git fetch upstream --prune
fi

echo "--- Checking out ${PR_BRANCH}..."
git checkout -B "${PR_BRANCH}" "origin/${PR_BRANCH}"

echo "--- Rebasing onto ${REBASE_TARGET}..."
# Rebase needs special handling — abort on failure instead of leaving a dirty state
if ! git rebase "${REBASE_TARGET}"; then
    echo ""
    echo "!!! Rebase had conflicts — aborting."
    git rebase --abort 2>/dev/null || true
    echo ""
    echo "Press Enter to return to sidero-rebaser..."
    read -r
    exit 1
fi

echo "--- Force pushing to origin/${PR_BRANCH}..."
git push --force-with-lease origin "${PR_BRANCH}"

echo ""
echo "==> Rebase complete!"
echo ""
for i in $(seq %[9]d -1 1); do
    printf "\rReturning in %%d..." "$i"
    sleep 1
done
echo ""
`, repoPath, forkURL, upstreamURL, pr.HeadBranch, pr.BaseBranch, isFork, rebaseTarget, gitconfigPath(), countdown), nil
}
