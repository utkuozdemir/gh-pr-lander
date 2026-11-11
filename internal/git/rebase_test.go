package git

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/utkuozdemir/gh-pr-lander/internal/pr"
)

func TestBuildRebaseScriptIsWellFormed(t *testing.T) {
	script, err := BuildRebaseScript("/tmp/data", pr.PR{
		RepoOwner:     "siderolabs",
		RepoName:      "talos",
		HeadRepoOwner: "utkuozdemir",
		HeadRepoName:  "talos",
		HeadBranch:    "fix-thing",
		BaseBranch:    "main",
	}, 3)
	if err != nil {
		t.Fatalf("BuildRebaseScript: %v", err)
	}

	// Guard against a formatter ever mangling the embedded bash: every `if`
	// (each ends in `; then`) must have a matching `fi` line. This is exactly
	// the kind of corruption that a long-line reformatter introduced once.
	thens := strings.Count(script, "; then")

	fis := 0

	for line := range strings.SplitSeq(script, "\n") {
		if strings.TrimSpace(line) == "fi" {
			fis++
		}
	}

	if thens != fis {
		t.Fatalf("unbalanced if/fi: %d 'then' vs %d 'fi'\n%s", thens, fis, script)
	}

	for _, want := range []string{
		"git fetch upstream --prune",
		`git rebase "${REBASE_TARGET}"`,
		`git push --force-with-lease origin "${PR_BRANCH}"`,
		"REBASE_TARGET='upstream/main'",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q\n%s", want, script)
		}
	}
}

func TestBuildRebaseScriptKeepsBranchNamesLiteral(t *testing.T) {
	// Git allows $, backticks and quotes in branch names, and a taken-over PR's
	// branch name comes from a contributor. It must never be evaluated.
	branch := "fix/$(echo injected)`echo injected`'\"$HOME"

	script, err := BuildRebaseScript(t.TempDir(), pr.PR{
		RepoOwner:     "siderolabs",
		RepoName:      "talos",
		HeadRepoOwner: "contributor",
		HeadRepoName:  "talos",
		HeadBranch:    branch,
		BaseBranch:    "main",
	}, 3)
	if err != nil {
		t.Fatalf("BuildRebaseScript: %v", err)
	}

	var assignment string

	for line := range strings.SplitSeq(script, "\n") {
		if strings.HasPrefix(line, "PR_BRANCH=") {
			assignment = line
		}
	}

	out, err := exec.CommandContext(t.Context(), "bash", "-c", assignment+`; printf %s "$PR_BRANCH"`).Output()
	if err != nil {
		t.Fatalf("bash: %v", err)
	}

	if string(out) != branch {
		t.Fatalf("expected branch %q to stay literal, bash produced %q", branch, out)
	}
}

func TestBuildRebaseScriptRejectsUnknownFork(t *testing.T) {
	if _, err := BuildRebaseScript("/tmp/data", pr.PR{}, 3); err == nil {
		t.Fatal("expected error when head repo is unknown")
	}
}
