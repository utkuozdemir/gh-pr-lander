package engine

import (
	"slices"
	"testing"
)

func TestAutomergeWaitsForItsConditions(t *testing.T) {
	p := readyPR("sha1")
	p.Approvals = 1
	p.ReviewThreads = 3
	p.ResolvedReviewThreads = 1
	key := p.RepoShort()

	m := testState(t, p)
	m.AutoMerge[key] = true

	if !m.shouldAutoMerge(p) {
		t.Fatal("expected automerge with no condition to merge a ready PR")
	}

	if _, err := m.SetMinApprovals(p, 2); err != nil {
		t.Fatal(err)
	}

	if _, err := m.ToggleResolvedOnly(p); err != nil {
		t.Fatal(err)
	}

	want := []string{"1 more approval", "2 unresolved comments"}
	if got := m.AutomergeWaitsFor(p); !slices.Equal(got, want) {
		t.Fatalf("expected automerge to wait for %v, got %v", want, got)
	}

	if m.shouldAutoMerge(p) || m.autoMergeWork(p) != nil {
		t.Fatal("expected automerge to hold back while its conditions are not met")
	}

	// The owner can still merge: GitHub would accept it.
	if err := m.CheckMerge(p); err != nil {
		t.Fatalf("expected a merge the owner starts to go ahead, got %v", err)
	}

	p.Approvals = 2
	p.ResolvedReviewThreads = 3

	if len(m.AutomergeWaitsFor(p)) != 0 || !m.shouldAutoMerge(p) {
		t.Fatal("expected automerge to merge once two people approve and every comment is resolved")
	}
}

func TestLooseningAConditionMergesRightAway(t *testing.T) {
	p := readyPR("sha1")
	p.Approvals = 1

	m := testState(t, p)
	m.AutoMerge[p.RepoShort()] = true

	if work, err := m.SetMinApprovals(p, 2); err != nil || work != nil {
		t.Fatalf("expected no merge while waiting for a second approval, got %v, %v", work != nil, err)
	}

	if work, err := m.SetMinApprovals(p, 1); err != nil || work == nil {
		t.Fatalf("expected a merge once the condition is dropped, got %v, %v", work != nil, err)
	}
}

func TestApprovalsConditionStopsAtThree(t *testing.T) {
	p := readyPR("sha1")
	key := p.RepoShort()

	m := testState(t, p)

	if _, err := m.SetMinApprovals(p, 9); err != nil || m.MinApprovals[key] != MaxApprovals {
		t.Fatalf("expected at most %d approvals, got %d (%v)", MaxApprovals, m.MinApprovals[key], err)
	}
}

// Turning automerge on for a ready PR merges it at once, so the conditions have
// to be settable before that, and they stay with the PR when automerge goes
// off again.
func TestConditionsAreSetBeforeAutomergeAndStayWithThePR(t *testing.T) {
	p := readyPR("sha1")
	p.Approvals = 1
	key := p.RepoShort()

	m := testState(t, p)

	if work, err := m.SetMinApprovals(p, 2); err != nil || work != nil {
		t.Fatalf(
			"expected the condition to be set with automerge off and nothing to start, got %v, %v",
			work != nil,
			err,
		)
	}

	enabled, work, err := m.Toggle(Automerge, p)
	if err != nil || !enabled || work != nil {
		t.Fatalf(
			"expected automerge to go on without merging the ready PR, got on=%v merge=%v err=%v",
			enabled,
			work != nil,
			err,
		)
	}

	if _, _, err := m.Toggle(Automerge, p); err != nil {
		t.Fatal(err)
	}

	if m.MinApprovals[key] != 2 {
		t.Fatalf("expected the condition to stay after automerge went off, got %d", m.MinApprovals[key])
	}

	reloaded, err := New(m.Config, m.Client)
	if err != nil {
		t.Fatal(err)
	}

	if reloaded.MinApprovals[key] != 2 {
		t.Fatalf("expected the condition to be saved, got %v", reloaded.MinApprovals)
	}
}

// A note of the owner asks for nothing, so "comments resolved" does not wait
// for it.
func TestNotesDoNotHoldBackAutomerge(t *testing.T) {
	p := readyPR("sha1")
	p.ReviewThreads, p.Notes = 2, 1
	key := p.RepoShort()

	m := testState(t, p)
	m.AutoMerge[key] = true
	m.ResolvedOnly[key] = true

	if got := m.AutomergeWaitsFor(p); len(got) != 1 || got[0] != "1 unresolved comment" {
		t.Fatalf("expected automerge to wait for the one real comment, got %v", got)
	}

	p.ResolvedReviewThreads = 1

	if !m.shouldAutoMerge(p) {
		t.Fatal("expected automerge to merge with only a note left open")
	}
}
