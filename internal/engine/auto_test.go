package engine

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

func settlingModel() *State {
	return &State{
		Config:           config.Config{PollInterval: 2 * time.Minute},
		AutoMerge:        map[string]bool{"org/repo#1": true},
		MergeFailed:      map[string]bool{},
		LastAutoCheck:    map[string]time.Time{},
		RerunInFlight:    map[string]time.Time{"org/repo#1": time.Now()},
		RerunInFlightSHA: map[string]string{"org/repo#1": "sha1"},
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

	if _, ok := m.RerunInFlight["org/repo#1"]; !ok {
		t.Fatal("in-flight guard should remain while still settling")
	}
}

func TestApplyRerunSettlingBlocksSpuriousAutomerge(t *testing.T) {
	m := settlingModel()

	prs := m.applyRerunSettling([]pr.PR{readyPR("sha1")})
	m.PRs = prs

	if m.shouldAutoMerge(prs[0]) {
		t.Fatal("automerge must not fire while a re-run is settling")
	}

	if works := m.AutoWorks(); len(works) != 0 {
		t.Fatalf("expected no automation during settling, got %d", len(works))
	}
}

func TestApplyRerunSettlingClearsWhenRunningObserved(t *testing.T) {
	m := settlingModel()

	running := readyPR("sha1")
	running.CIStatus = pr.CIRunning
	running.OverallStatus = pr.ComputeOverallStatus(running)

	m.applyRerunSettling([]pr.PR{running})

	if _, ok := m.RerunInFlight["org/repo#1"]; ok {
		t.Fatal("in-flight guard should clear once GitHub reports running again")
	}
}

func TestApplyRerunSettlingClearsOnNewPush(t *testing.T) {
	m := settlingModel()

	got := m.applyRerunSettling([]pr.PR{readyPR("sha2")}) // different SHA

	if _, ok := m.RerunInFlight["org/repo#1"]; ok {
		t.Fatal("a new push (changed SHA) should clear the in-flight guard")
	}

	if got[0].OverallStatus != pr.StatusReady {
		t.Fatal("after a new push the genuine ready state should be preserved")
	}
}

func TestApplyRerunSettlingClearsAfterGrace(t *testing.T) {
	m := settlingModel()
	// Trigger time well beyond the settle grace.
	m.RerunInFlight["org/repo#1"] = time.Now().Add(-10 * time.Minute)

	got := m.applyRerunSettling([]pr.PR{readyPR("sha1")})

	if _, ok := m.RerunInFlight["org/repo#1"]; ok {
		t.Fatal("in-flight guard should clear after the grace window elapses")
	}

	if got[0].OverallStatus != pr.StatusReady {
		t.Fatal("after grace, the genuine ready state should be trusted")
	}
}

func TestMarkRerunInFlightViaRerunMessage(t *testing.T) {
	m := testState(t, readyPR("sha1"))

	m.RerunDone(RerunResult{PR: pr.PR{Repo: "org/repo", Number: 1, HeadSHA: "sha1"}})

	if _, ok := m.RerunInFlight["org/repo#1"]; !ok {
		t.Fatal("a successful manual re-run should mark the PR in flight")
	}
}

// Checks settling (issue #7): an external rebase+push makes GitHub briefly
// report a small, all-green check set on the new commit before all its required
// checks register. The PR must not be treated as ready during that window.

func checksSettlingModel() *State {
	return &State{
		Config:        config.Config{PollInterval: 2 * time.Minute},
		AutoMerge:     map[string]bool{"org/repo#1": true},
		MergeFailed:   map[string]bool{},
		LastAutoCheck: map[string]time.Time{},
		CheckBaseline: map[string]CheckBaseline{},
	}
}

// checkedPR is a ready PR carrying a given total number of (passed) checks.
func checkedPR(sha string, total int) pr.PR {
	p := readyPR(sha)
	p.Checks = pr.CheckSummary{Total: total, Passed: total}

	return p
}

func TestApplyChecksSettlingSuppressesPostPushUnderreport(t *testing.T) {
	m := checksSettlingModel()
	// Baseline: the previous commit had a full set of green checks.
	m.applyChecksSettling([]pr.PR{checkedPR("sha1", 14)})

	// Rebase+push: the new commit transiently reports a single passing check.
	got := m.applyChecksSettling([]pr.PR{checkedPR("sha2", 1)})

	if got[0].CIStatus != pr.CIRunning {
		t.Fatalf("expected CIRunning while the new commit's checks register, got %v", got[0].CIStatus)
	}

	if got[0].OverallStatus == pr.StatusReady {
		t.Fatal("PR must not be READY while its post-push checks are settling")
	}
}

func TestApplyChecksSettlingClearsWhenChecksCatchUp(t *testing.T) {
	m := checksSettlingModel()
	m.applyChecksSettling([]pr.PR{checkedPR("sha1", 14)})
	m.applyChecksSettling([]pr.PR{checkedPR("sha2", 1)}) // suppressed

	got := m.applyChecksSettling([]pr.PR{checkedPR("sha2", 14)}) // fully re-registered

	if got[0].CIStatus != pr.CIPassed {
		t.Fatalf("expected CIPassed once all checks re-registered, got %v", got[0].CIStatus)
	}
}

func TestApplyChecksSettlingIgnoresFirstSighting(t *testing.T) {
	m := checksSettlingModel()

	// No prior baseline, so a small green set is trusted (a genuinely green PR
	// on first load must show as ready).
	got := m.applyChecksSettling([]pr.PR{checkedPR("sha1", 1)})

	if got[0].CIStatus != pr.CIPassed {
		t.Fatalf("a first sighting must be trusted, got %v", got[0].CIStatus)
	}
}

func TestApplyChecksSettlingClearsAfterGrace(t *testing.T) {
	m := checksSettlingModel()
	m.applyChecksSettling([]pr.PR{checkedPR("sha1", 14)})
	m.applyChecksSettling([]pr.PR{checkedPR("sha2", 1)})

	// Age the new commit's first-seen time beyond the grace window.
	bl := m.CheckBaseline["org/repo#1"]
	bl.FirstSeen = time.Now().Add(-m.Config.RerunSettleGrace() - time.Second)
	m.CheckBaseline["org/repo#1"] = bl

	got := m.applyChecksSettling([]pr.PR{checkedPR("sha2", 1)})

	if got[0].CIStatus != pr.CIPassed {
		t.Fatalf("after the grace window the reading must be trusted, got %v", got[0].CIStatus)
	}
}

func TestApplyChecksSettlingBlocksSpuriousAutomerge(t *testing.T) {
	m := checksSettlingModel()
	m.applyChecksSettling([]pr.PR{checkedPR("sha1", 14)})
	prs := m.applyChecksSettling([]pr.PR{checkedPR("sha2", 1)})

	if m.shouldAutoMerge(prs[0]) {
		t.Fatal("automerge must not fire while the post-push checks are settling")
	}
}
