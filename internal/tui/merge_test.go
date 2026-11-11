package tui

import (
	"errors"
	"fmt"
	"testing"

	"github.com/utkuozdemir/sidero-rebaser/internal/github"
	"github.com/utkuozdemir/sidero-rebaser/internal/pr"
)

// A merge GitHub refused is not asked for again on the same commit. A new head
// commit (a rebase) gets a new try.
func TestRefusedAutomergeWaitsForNewHeadCommit(t *testing.T) {
	p := pr.PR{
		Repo: "org/repo", Number: 1, BaseBranch: "main", HeadSHA: "aaa", OverallStatus: pr.StatusReady,
	}
	key := p.RepoShort()

	m := testAutoToggleModel(t, p)
	m.client = fakeClient{mergeErr: fmt.Errorf("%w: required status check is expected", github.ErrMergeRefused)}
	m.autoMerge[key] = true

	cmd := m.autoMergeCmd(p)
	if cmd == nil {
		t.Fatal("expected automerge to merge a ready PR")
	}

	msg, ok := cmd().(mergeResultMsg)
	if !ok || !msg.auto || msg.err == nil {
		t.Fatalf("expected a failed automerge result, got %#v", msg)
	}

	updated, _ := m.Update(msg)
	m = updated.(model)

	if m.autoMergeCmd(p) != nil {
		t.Fatal("expected no second merge attempt on the refused commit")
	}

	rebased := p
	rebased.HeadSHA = "bbb"
	m.prevPRs = []pr.PR{p}
	m.prs = []pr.PR{rebased}
	m.checkTransitions()

	if m.autoMergeCmd(rebased) == nil {
		t.Fatal("expected automerge to try again on the new head commit")
	}
}

func TestManualMergeNeedsReadyPR(t *testing.T) {
	p := pr.PR{Repo: "org/repo", Number: 1, BaseBranch: "main", HeadSHA: "aaa", OverallStatus: pr.StatusStale}
	m := testAutoToggleModel(t, p)

	if _, cmd := m.doMerge(); cmd == nil {
		t.Fatal("expected a status-clearing command")
	} else if _, merged := cmd().(mergeResultMsg); merged {
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

	m := testAutoToggleModel(t, p)
	m.autoMerge[p.RepoShort()] = true

	for _, err := range []error{github.ErrNotFastForward, errors.New("fast-forward merge HTTP 502: bad gateway")} {
		updated, _ := m.Update(mergeResultMsg{pr: p, auto: true, err: err})
		m = updated.(model)

		if m.isMergeFailed(p.RepoShort()) {
			t.Fatalf("expected automerge to stay on after %q", err)
		}
	}
}
