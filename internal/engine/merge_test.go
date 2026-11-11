package engine

import (
	"errors"
	"fmt"
	"testing"

	"github.com/utkuozdemir/gh-pr-lander/internal/github"
	"github.com/utkuozdemir/gh-pr-lander/internal/pr"
)

// A merge GitHub refused is not asked for again on the same commit. A new head
// commit (a rebase) gets a new try.
func TestRefusedAutomergeWaitsForNewHeadCommit(t *testing.T) {
	p := pr.PR{
		Repo: "org/repo", Number: 1, BaseBranch: "main", HeadSHA: "aaa", OverallStatus: pr.StatusReady,
	}
	key := p.RepoShort()

	m := testState(t, p)
	m.Client = fakeClient{mergeErr: fmt.Errorf("%w: required status check is expected", github.ErrMergeRefused)}
	m.AutoMerge[key] = true

	cmd := m.autoMergeWork(p)
	if cmd == nil {
		t.Fatal("expected automerge to merge a ready PR")
	}

	res, ok := cmd().(MergeResult)
	if !ok || !res.Auto || res.Err == nil {
		t.Fatalf("expected a failed automerge result, got %#v", res)
	}

	m.MergeDone(res)

	if m.autoMergeWork(p) != nil {
		t.Fatal("expected no second merge attempt on the refused commit")
	}

	rebased := p
	rebased.HeadSHA = "bbb"
	m.PrevPRs = []pr.PR{p}
	m.PRs = []pr.PR{rebased}
	m.checkTransitions()

	if m.autoMergeWork(rebased) == nil {
		t.Fatal("expected automerge to try again on the new head commit")
	}
}

func TestManualMergeNeedsReadyPR(t *testing.T) {
	p := pr.PR{Repo: "org/repo", Number: 1, BaseBranch: "main", HeadSHA: "aaa", OverallStatus: pr.StatusStale}
	m := testState(t, p)

	if err := m.CheckMerge(p); err == nil {
		t.Fatal("expected no merge of a PR that is not ready")
	}
}

// Only a refusal is about the commit. Losing the race to another merge shows
// the PR as behind on the next fetch, and a network or server error is retried,
// so automerge does not give up on either.
func TestAutomergeGivesUpOnlyOnRefusal(t *testing.T) {
	p := pr.PR{
		Repo: "org/repo", Number: 1, BaseBranch: "main", HeadSHA: "aaa", OverallStatus: pr.StatusReady,
	}

	m := testState(t, p)
	m.AutoMerge[p.RepoShort()] = true

	for _, err := range []error{github.ErrNotFastForward, errors.New("fast-forward merge HTTP 502: bad gateway")} {
		m.MergeDone(MergeResult{PR: p, Auto: true, Err: err})

		if m.IsMergeFailed(p.RepoShort()) {
			t.Fatalf("expected automerge to stay on after %q", err)
		}
	}
}
