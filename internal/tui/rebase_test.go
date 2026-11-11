package tui

import (
	"errors"
	"strings"
	"testing"

	"github.com/utkuozdemir/sidero-rebaser/internal/config"
	"github.com/utkuozdemir/sidero-rebaser/internal/engine"
	"github.com/utkuozdemir/sidero-rebaser/internal/pr"
)

// queuePR is an approved, green PR on org/repo main with the given base state.
func queuePR(number int, base pr.BaseStatus) pr.PR {
	return pr.PR{
		Repo:         "org/repo",
		Number:       number,
		BaseBranch:   "main",
		HeadSHA:      "sha" + strings.Repeat("x", number),
		BaseStatus:   base,
		CIStatus:     pr.CIPassed,
		ReviewStatus: pr.ReviewApproved,
	}
}

func queueModel(t *testing.T, prs []pr.PR, order map[string]int) model {
	t.Helper()

	return model{
		State: &engine.State{
			Config:     config.Config{DataDir: t.TempDir()},
			PRs:        prs,
			AutoRebase: order,
		},
		rebaseState: map[string]autoRebaseState{},
	}
}

func nextKey(m model) string {
	p, ok := m.nextAutoRebase()
	if !ok {
		return ""
	}

	return p.RepoShort()
}

func TestNextAutoRebaseRebasesOnlyTheFirstInQueue(t *testing.T) {
	m := queueModel(t,
		[]pr.PR{queuePR(1, pr.BaseBehind), queuePR(2, pr.BaseBehind)},
		map[string]int{"org/repo#1": 2, "org/repo#2": 1},
	)

	if got := nextKey(m); got != "org/repo#2" {
		t.Fatalf("expected the first in queue order, got %q", got)
	}
}

func TestNextAutoRebaseWaitsForThePRInFlight(t *testing.T) {
	// #2 is later in the queue but already up to date with CI running, so it
	// holds the queue and #1 must not be rebased into a CI race with it.
	inFlight := queuePR(2, pr.BaseSynced)
	inFlight.CIStatus = pr.CIRunning

	m := queueModel(t,
		[]pr.PR{queuePR(1, pr.BaseBehind), inFlight},
		map[string]int{"org/repo#1": 1, "org/repo#2": 2},
	)

	if got := nextKey(m); got != "" {
		t.Fatalf("expected no rebase while a PR is in flight, got %q", got)
	}

	if slot := m.queueSlots()["org/repo#2"]; !slot.active || slot.pos != 2 {
		t.Fatalf("expected #2 to be active at position 2, got %+v", slot)
	}
}

func TestNextAutoRebaseSkipsPRsThatCannotMerge(t *testing.T) {
	unapproved := queuePR(1, pr.BaseBehind)
	unapproved.ReviewStatus = pr.ReviewPending

	failing := queuePR(2, pr.BaseBehind)
	failing.CIStatus = pr.CIFailed

	m := queueModel(t,
		[]pr.PR{unapproved, failing, queuePR(3, pr.BaseBehind)},
		map[string]int{"org/repo#1": 1, "org/repo#2": 2, "org/repo#3": 3},
	)

	if got := nextKey(m); got != "org/repo#3" {
		t.Fatalf("expected the first PR that could merge, got %q", got)
	}
}

func TestNextAutoRebaseRunsOneAtATimeAcrossQueues(t *testing.T) {
	release := queuePR(2, pr.BaseBehind)
	release.BaseBranch = "release-1.0"

	m := queueModel(t,
		[]pr.PR{queuePR(1, pr.BaseBehind), release},
		map[string]int{"org/repo#1": 1, "org/repo#2": 2},
	)

	if got := nextKey(m); got == "" {
		t.Fatal("expected a rebase to start")
	}

	m.rebasing = "org/repo#1"

	if got := nextKey(m); got != "" {
		t.Fatalf("expected no second rebase while one runs, got %q", got)
	}
}

func TestAutoRebaseDoesNotRepeatUntilGitHubCatchesUp(t *testing.T) {
	p := queuePR(1, pr.BaseBehind)
	m := queueModel(t, []pr.PR{p}, map[string]int{"org/repo#1": 1})
	m.rebasing = "org/repo#1"

	updated, _ := m.handleAutoRebaseResult(autoRebaseResultMsg{pr: p})
	m = updated.(model)

	if m.rebasing != "" {
		t.Fatalf("expected the running marker to clear, got %q", m.rebasing)
	}

	// The next fetch can still show the old commit as behind.
	if got := nextKey(m); got != "" {
		t.Fatalf("expected no rebase of the same commit again, got %q", got)
	}

	// Behind again on a new commit: someone merged in the meantime.
	m.PRs[0].HeadSHA = "rebased"
	if got := nextKey(m); got != "org/repo#1" {
		t.Fatalf("expected a rebase of the new commit, got %q", got)
	}
}

func TestAutoRebaseFailurePausesTheQueueUntilRetried(t *testing.T) {
	m := queueModel(t,
		[]pr.PR{queuePR(1, pr.BaseBehind), queuePR(2, pr.BaseBehind)},
		map[string]int{"org/repo#1": 1, "org/repo#2": 2},
	)
	m.rebasing = "org/repo#1"

	updated, _ := m.handleAutoRebaseResult(autoRebaseResultMsg{
		pr:     m.PRs[0],
		output: "Rebasing (1/1)error: gpg failed to sign the data:\nerror: failed to write commit object\n",
		err:    errors.New("exit status 1"),
	})
	m = updated.(model)

	// No retry, and #2 must not be rebased while the owner is away either.
	if m.rebasing != "" {
		t.Fatalf("expected no rebase after a failure, %q started", m.rebasing)
	}

	if out := m.renderActivePR(m.PRs[0], false); !strings.Contains(out, "rebase failed: signing failed") {
		t.Fatalf("expected the failure reason in %q", out)
	}

	// b on the failed PR retries it and keeps autorebase on.
	m.cursor = 0

	updated, _ = m.toggleAutoRebase()
	m = updated.(model)

	if _, on := m.AutoRebase["org/repo#1"]; !on || m.rebasing != "org/repo#1" {
		t.Fatalf("expected b to retry #1, rebasing %q", m.rebasing)
	}
}

func TestAutoRebaseFailureClearsOnNewCommit(t *testing.T) {
	m := queueModel(t, []pr.PR{queuePR(1, pr.BaseBehind)}, map[string]int{"org/repo#1": 1})

	updated, _ := m.handleAutoRebaseResult(autoRebaseResultMsg{pr: m.PRs[0], err: errors.New("exit status 1")})
	m = updated.(model)

	if got := nextKey(m); got != "" {
		t.Fatalf("expected the failed PR to wait, got %q", got)
	}

	// A manual rebase or an outside push moves the head.
	m.PRs[0].HeadSHA = "pushed"
	if got := nextKey(m); got != "org/repo#1" {
		t.Fatalf("expected the new commit to be rebased, got %q", got)
	}
}

func TestFailureReason(t *testing.T) {
	for output, want := range map[string]string{
		"error: gpg failed to sign the data:\nerror: failed to write commit object": "signing failed, YubiKey touch missed?",
		"error: failed to push some refs to 'git@github.com:x/y.git'":               "push rejected",
		"CONFLICT (content): Merge conflict in a.go":                                "conflicts",
		"ssh: connect to host github.com port 22: Operation timed out":              "see the log",
	} {
		if got := failureReason(output); got != want {
			t.Errorf("failureReason(%q) = %q, want %q", output, got, want)
		}
	}
}

func TestMoveInQueueSwapsOrder(t *testing.T) {
	m := queueModel(t,
		[]pr.PR{queuePR(1, pr.BaseSynced), queuePR(2, pr.BaseBehind), queuePR(3, pr.BaseBehind)},
		map[string]int{"org/repo#1": 1, "org/repo#2": 2, "org/repo#3": 3},
	)
	m.cursor = 2 // #3

	updated, _ := m.moveInQueue(-1)
	m = updated.(model)

	slots := m.queueSlots()
	if slots["org/repo#3"].pos != 2 || slots["org/repo#2"].pos != 3 {
		t.Fatalf("expected #3 and #2 to swap, got %+v", slots)
	}

	// Moving to the top does not interrupt #1, which is in flight.
	updated, _ = m.moveInQueue(-1)
	m = updated.(model)

	if slots := m.queueSlots(); slots["org/repo#3"].pos != 1 || !slots["org/repo#1"].active {
		t.Fatalf("expected #3 first and #1 still active, got %+v", slots)
	}

	if m.rebasing != "" {
		t.Fatalf("expected no rebase while #1 is in flight, %q started", m.rebasing)
	}
}

func TestToggleAutoRebaseAppendsToQueue(t *testing.T) {
	m := queueModel(t,
		[]pr.PR{queuePR(1, pr.BaseSynced), queuePR(2, pr.BaseSynced)},
		map[string]int{"org/repo#1": 7},
	)
	m.cursor = 1

	updated, _ := m.toggleAutoRebase()
	m = updated.(model)

	if slot := m.queueSlots()["org/repo#2"]; slot.pos != 2 || slot.total != 2 {
		t.Fatalf("expected #2 at the end of the queue, got %+v", slot)
	}

	updated, _ = m.toggleAutoRebase()
	m = updated.(model)

	if _, on := m.AutoRebase["org/repo#2"]; on {
		t.Fatal("expected autorebase to be off again")
	}
}

func TestToggleAutoRebaseStartsRightAway(t *testing.T) {
	m := queueModel(t, []pr.PR{queuePR(1, pr.BaseBehind)}, map[string]int{})

	updated, cmd := m.toggleAutoRebase()
	m = updated.(model)

	if m.rebasing != "org/repo#1" || cmd == nil {
		t.Fatalf("expected the rebase to start and be marked running, got %q", m.rebasing)
	}
}

func TestRenderShowsQueuePosition(t *testing.T) {
	m := queueModel(t,
		[]pr.PR{queuePR(1, pr.BaseBehind), queuePR(2, pr.BaseBehind)},
		map[string]int{"org/repo#1": 1, "org/repo#2": 2},
	)
	m.width = 120

	if out := m.renderActivePR(m.PRs[0], false); !strings.Contains(out, "▶ 1/2") {
		t.Fatalf("expected the active marker in %q", out)
	}

	if out := m.renderActivePR(m.PRs[1], false); !strings.Contains(out, "⏳ 2/2") {
		t.Fatalf("expected the waiting marker in %q", out)
	}
}

func TestReorderDuringRebaseDoesNotStartASecondOne(t *testing.T) {
	m := queueModel(t,
		[]pr.PR{queuePR(1, pr.BaseBehind), queuePR(2, pr.BaseBehind)},
		map[string]int{"org/repo#1": 1, "org/repo#2": 2},
	)
	m.rebasing = "org/repo#1"
	m.cursor = 1 // #2

	updated, _ := m.moveInQueue(-1)
	m = updated.(model)

	if slot := m.queueSlots()["org/repo#1"]; !slot.active {
		t.Fatalf("expected #1 to stay active while it rebases, got %+v", slot)
	}

	// #1 succeeds, and the next fetch still shows both old heads as behind.
	updated, _ = m.handleAutoRebaseResult(autoRebaseResultMsg{pr: m.PRs[0]})
	m = updated.(model)

	if got := nextKey(m); got != "" {
		t.Fatalf("expected #1 to keep the queue until GitHub catches up, got %q", got)
	}
}

func TestFlakyFailureUnderAutorerunKeepsTheQueue(t *testing.T) {
	flaky := queuePR(1, pr.BaseSynced)
	flaky.CIStatus = pr.CIFailed
	flaky.FailedWorkflowRunsKnown = true
	flaky.FailedWorkflowRuns = 1

	m := queueModel(t,
		[]pr.PR{flaky, queuePR(2, pr.BaseBehind)},
		map[string]int{"org/repo#1": 1, "org/repo#2": 2},
	)
	m.AutoRerun = map[string]bool{"org/repo#1": true}

	if got := nextKey(m); got != "" {
		t.Fatalf("expected #1 to hold the queue while autorerun handles it, got %q", got)
	}

	// Without an Actions run behind the failure, autorerun cannot help.
	m.PRs[0].FailedWorkflowRuns = 0
	if got := nextKey(m); got != "org/repo#2" {
		t.Fatalf("expected the queue to move on to #2, got %q", got)
	}
}

func TestManualRebaseReservesTheClone(t *testing.T) {
	m := queueModel(t, []pr.PR{queuePR(1, pr.BaseBehind)}, map[string]int{"org/repo#1": 1})
	m.PRs[0].RepoOwner, m.PRs[0].RepoName = "org", "repo"
	m.PRs[0].HeadRepoOwner, m.PRs[0].HeadRepoName = "me", "repo"

	updated, cmd := m.doRebase()
	m = updated.(model)

	if cmd == nil || m.rebasing != "org/repo#1" {
		t.Fatalf("expected the manual rebase to reserve the clone, got %q", m.rebasing)
	}

	if got := nextKey(m); got != "" {
		t.Fatalf("expected no autorebase while the manual one runs, got %q", got)
	}

	updated, _ = m.doRebase()
	if got := updated.(model).status; !strings.Contains(got, "is running") {
		t.Fatalf("expected a second rebase to be refused, got %q", got)
	}

	updated, _ = m.Update(rebaseFinishedMsg{})
	if got := updated.(model).rebasing; got != "" {
		t.Fatalf("expected the reservation to clear, got %q", got)
	}
}
