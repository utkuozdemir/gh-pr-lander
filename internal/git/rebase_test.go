package git

import (
	"strings"
	"testing"

	"github.com/utkuozdemir/sidero-rebaser/internal/pr"
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
		"REBASE_TARGET=\"upstream/main\"",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q\n%s", want, script)
		}
	}
}

func TestBuildRebaseScriptRejectsUnknownFork(t *testing.T) {
	if _, err := BuildRebaseScript("/tmp/data", pr.PR{}, 3); err == nil {
		t.Fatal("expected error when head repo is unknown")
	}
}
