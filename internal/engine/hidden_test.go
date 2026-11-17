package engine

import (
	"testing"
	"time"

	"github.com/utkuozdemir/gh-pr-lander/internal/pr"
)

func TestHidingChangesOnlyWhatIsListed(t *testing.T) {
	old := pr.PR{Repo: "org/repo", Number: 1, OverallStatus: pr.StatusReady}
	s := testState(t, old)
	s.PRs = append(s.PRs, pr.PR{Repo: "org/repo", Number: 2})
	s.AutoMerge[old.RepoShort()] = true

	hidden, err := s.ToggleHidden(old)
	if err != nil || !hidden {
		t.Fatalf("expected the PR to be hidden, got %v, error %v", hidden, err)
	}

	listed, hiddenPRs := s.Shown()
	if len(listed) != 1 || listed[0].Number != 2 || len(hiddenPRs) != 1 || hiddenPRs[0].Number != 1 {
		t.Fatalf("expected #2 listed and #1 hidden, got %v and %v", listed, hiddenPRs)
	}

	// Still watched: automerge on a hidden PR merges it when it is ready.
	if !s.shouldAutoMerge(old) {
		t.Fatal("expected automerge to go on for a hidden PR")
	}

	// It survives a restart.
	reloaded, err := New(s.Config, s.Client)
	if err != nil || !reloaded.Hidden[old.RepoShort()] {
		t.Fatalf("expected the hidden PR to be saved, got %v, error %v", reloaded.Hidden, err)
	}

	if hidden, _ := s.ToggleHidden(old); hidden {
		t.Fatal("expected a second toggle to show the PR again")
	}
}

func TestHiddenPRIsForgottenWhenItIsGone(t *testing.T) {
	p := pr.PR{Repo: "org/repo", Number: 1}
	s := testState(t, p)
	s.RecentlyMerged = map[string]*MergedPR{}

	if _, err := s.ToggleHidden(p); err != nil {
		t.Fatal(err)
	}

	s.PRs = nil
	s.RecentlyMerged[p.RepoShort()] = &MergedPR{PR: p, GoneAt: time.Now(), Confirmed: true, Merged: true}

	if err := s.cleanupAuto(); err != nil {
		t.Fatal(err)
	}

	if len(s.Hidden) != 0 {
		t.Fatalf("expected the merged PR to be forgotten, got %v", s.Hidden)
	}
}
