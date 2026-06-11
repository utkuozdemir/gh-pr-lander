package tui

import (
	"testing"
	"time"

	"github.com/utkuozdemir/sidero-rebaser/internal/config"
	"github.com/utkuozdemir/sidero-rebaser/internal/pr"
)

// readyPR is a PR that, on its own, would be considered ready to merge.
func readyPR(sha string) pr.PR {
	p := pr.PR{
		Repo:         "org/repo",
		Number:       1,
		HeadSHA:      sha,
		BaseStatus:   pr.BaseSynced,
		CIStatus:     pr.CIPassed,
		ReviewStatus: pr.ReviewApproved,
	}
	p.OverallStatus = pr.ComputeOverallStatus(p)

	return p
}

func settlingModel() model {
	return model{
		config:           config.Config{PollInterval: 2 * time.Minute},
		autoMerge:        map[string]bool{"org/repo#1": true},
		mergeFailed:      map[string]bool{},
		lastAutoCheck:    map[string]time.Time{},
		rerunInFlight:    map[string]time.Time{"org/repo#1": time.Now()},
		rerunInFlightSHA: map[string]string{"org/repo#1": "sha1"},
	}
}

// TestApplyRerunSettlingSuppressesTransientPass is the core regression test for
// issue #1: right after a re-run, GitHub may momentarily report all checks
// passed. The PR must not be treated as ready while the re-run is settling.
func TestApplyRerunSettlingSuppressesTransientPass(t *testing.T) {
	m := settlingModel()

	got := m.applyRerunSettling([]pr.PR{readyPR("sha1")})

	if got[0].CIStatus != pr.CIRunning {
		t.Fatalf("expected transient pass to be treated as running, got %v", got[0].CIStatus)
	}

	if got[0].OverallStatus == pr.StatusReady {
		t.Fatal("settling PR must not be READY")
	}

	if _, ok := m.rerunInFlight["org/repo#1"]; !ok {
		t.Fatal("in-flight guard should remain while still settling")
	}
}

func TestApplyRerunSettlingBlocksSpuriousAutomerge(t *testing.T) {
	m := settlingModel()

	prs := m.applyRerunSettling([]pr.PR{readyPR("sha1")})
	m.prs = prs

	if m.shouldAutoMerge(prs[0]) {
		t.Fatal("automerge must not fire while a re-run is settling")
	}

	if cmds := m.autoCmds(); len(cmds) != 0 {
		t.Fatalf("expected no auto commands during settling, got %d", len(cmds))
	}
}

func TestApplyRerunSettlingClearsWhenRunningObserved(t *testing.T) {
	m := settlingModel()

	running := readyPR("sha1")
	running.CIStatus = pr.CIRunning
	running.OverallStatus = pr.ComputeOverallStatus(running)

	m.applyRerunSettling([]pr.PR{running})

	if _, ok := m.rerunInFlight["org/repo#1"]; ok {
		t.Fatal("in-flight guard should clear once GitHub reports running again")
	}
}

func TestApplyRerunSettlingClearsOnNewPush(t *testing.T) {
	m := settlingModel()

	got := m.applyRerunSettling([]pr.PR{readyPR("sha2")}) // different SHA

	if _, ok := m.rerunInFlight["org/repo#1"]; ok {
		t.Fatal("a new push (changed SHA) should clear the in-flight guard")
	}

	if got[0].OverallStatus != pr.StatusReady {
		t.Fatal("after a new push the genuine ready state should be preserved")
	}
}

func TestApplyRerunSettlingClearsAfterGrace(t *testing.T) {
	m := settlingModel()
	// Trigger time well beyond the settle grace.
	m.rerunInFlight["org/repo#1"] = time.Now().Add(-10 * time.Minute)

	got := m.applyRerunSettling([]pr.PR{readyPR("sha1")})

	if _, ok := m.rerunInFlight["org/repo#1"]; ok {
		t.Fatal("in-flight guard should clear after the grace window elapses")
	}

	if got[0].OverallStatus != pr.StatusReady {
		t.Fatal("after grace, the genuine ready state should be trusted")
	}
}

func TestMarkRerunInFlightViaRerunMessage(t *testing.T) {
	m := testAutoToggleModel(t, readyPR("sha1"))

	updated, _ := m.Update(checksRerunMsg{pr: pr.PR{Repo: "org/repo", Number: 1, HeadSHA: "sha1"}})
	got := updated.(model)

	if _, ok := got.rerunInFlight["org/repo#1"]; !ok {
		t.Fatal("a successful manual re-run should mark the PR in flight")
	}
}
