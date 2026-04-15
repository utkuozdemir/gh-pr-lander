package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestModelIgnoresStalePRLoads(t *testing.T) {
	m := model{
		prs:           []PR{{Number: 1, Repo: "org/repo"}},
		latestFetchID: 2,
	}

	updated, _ := m.Update(prsLoadedMsg{
		requestID: 1,
		prs:       []PR{{Number: 2, Repo: "org/repo"}},
	})
	got := updated.(model)

	if len(got.prs) != 1 || got.prs[0].Number != 1 {
		t.Fatalf("stale load replaced PRs: %#v", got.prs)
	}
}

func TestMergedCheckErrorDoesNotConfirmClosed(t *testing.T) {
	key := "org/repo#1"
	m := model{
		recentlyMerged: map[string]*mergedPR{
			key: &mergedPR{PR: PR{Repo: "org/repo", Number: 1}, GoneAt: time.Now()},
		},
	}

	updated, _ := m.Update(mergedCheckMsg{prKey: key, err: errors.New("boom")})
	got := updated.(model)

	if got.recentlyMerged[key].Confirmed {
		t.Fatal("error result should not confirm merged/closed state")
	}
}

func TestMergedCheckOpenRemovesGoneEntry(t *testing.T) {
	key := "org/repo#1"
	m := model{
		recentlyMerged: map[string]*mergedPR{
			key: &mergedPR{PR: PR{Repo: "org/repo", Number: 1}, GoneAt: time.Now()},
		},
	}

	updated, _ := m.Update(mergedCheckMsg{prKey: key, open: true})
	got := updated.(model)

	if _, ok := got.recentlyMerged[key]; ok {
		t.Fatal("open PR should be removed from recentlyMerged")
	}
}

func TestRenderMergedItemUsesClosedDescription(t *testing.T) {
	key := "org/repo#1"
	m := model{
		width: 80,
		recentlyMerged: map[string]*mergedPR{
			key: &mergedPR{
				PR:        PR{Repo: "org/repo", Number: 1, Title: "closed"},
				GoneAt:    time.Now().Add(-time.Minute),
				Confirmed: true,
				Merged:    false,
			},
		},
	}

	out := m.renderMergedItem(displayItem{
		pr:     PR{Repo: "org/repo", Number: 1, Title: "closed"},
		merged: true,
		goneAt: m.recentlyMerged[key].GoneAt,
	}, false)

	if !strings.Contains(out, "CLOSED") || !strings.Contains(out, "Closed") {
		t.Fatalf("expected closed badge and description, got %q", out)
	}
	if strings.Contains(out, "Merged") {
		t.Fatalf("closed item should not say merged, got %q", out)
	}
}

func TestDoRerunChecksRejectsFailedNonActionsChecks(t *testing.T) {
	m := model{
		prs: []PR{{
			Repo:                    "org/repo",
			Number:                  1,
			CIStatus:                CIFailed,
			FailedWorkflowRunsKnown: true,
			FailedWorkflowRuns:      0,
		}},
	}

	updated, _ := m.doRerunChecks()
	got := updated.(model)

	if !strings.Contains(got.status, "No failed GitHub Actions jobs") {
		t.Fatalf("unexpected status: %q", got.status)
	}
}

func TestAutoCmdsUsesPersistedAutoRerunCooldown(t *testing.T) {
	m := model{
		config: Config{AutoCooldown: time.Hour},
		prs: []PR{{
			Repo:                    "org/repo",
			Number:                  1,
			CIStatus:                CIFailed,
			FailedWorkflowRunsKnown: true,
			FailedWorkflowRuns:      1,
		}},
		autoRerun:     map[string]bool{"org/repo#1": true},
		rerunFailed:   map[string]bool{},
		lastAutoCheck: map[string]time.Time{},
		lastAutoRerun: map[string]time.Time{"org/repo#1": time.Now()},
	}

	if cmds := m.autoCmds(); len(cmds) != 0 {
		t.Fatalf("expected cooldown to suppress autorerun, got %d commands", len(cmds))
	}
}

func TestToggleAutoMergeChecksImmediatelyWhenReady(t *testing.T) {
	key := "org/repo#1"
	m := testAutoToggleModel(t, PR{
		Repo:          "org/repo",
		Number:        1,
		OverallStatus: StatusReady,
	})

	updated, _ := m.toggleAuto(m.autoMerge, m.mergeFailed, "automerge")
	got := updated.(model)

	if !got.autoMerge[key] {
		t.Fatal("automerge was not enabled")
	}
	if got.lastAutoCheck["merge:"+key].IsZero() {
		t.Fatal("expected automerge to check immediately")
	}
}

func TestToggleAutoRerunChecksImmediatelyWhenEligible(t *testing.T) {
	key := "org/repo#1"
	m := testAutoToggleModel(t, PR{
		Repo:                    "org/repo",
		Number:                  1,
		CIStatus:                CIFailed,
		FailedWorkflowRunsKnown: true,
		FailedWorkflowRuns:      1,
	})

	updated, _ := m.toggleAuto(m.autoRerun, m.rerunFailed, "autorerun")
	got := updated.(model)

	if !got.autoRerun[key] {
		t.Fatal("autorerun was not enabled")
	}
	if got.lastAutoCheck["rerun:"+key].IsZero() {
		t.Fatal("expected autorerun to check immediately")
	}
}

func TestToggleAutoRerunRespectsPersistedCooldown(t *testing.T) {
	key := "org/repo#1"
	m := testAutoToggleModel(t, PR{
		Repo:                    "org/repo",
		Number:                  1,
		CIStatus:                CIFailed,
		FailedWorkflowRunsKnown: true,
		FailedWorkflowRuns:      1,
	})
	m.lastAutoRerun[key] = time.Now()

	updated, _ := m.toggleAuto(m.autoRerun, m.rerunFailed, "autorerun")
	got := updated.(model)

	if !got.autoRerun[key] {
		t.Fatal("autorerun was not enabled")
	}
	if !got.lastAutoCheck["rerun:"+key].IsZero() {
		t.Fatal("autorerun should not check immediately while persisted cooldown is active")
	}
}

func TestToggleAutoDisableClearsCheckCooldown(t *testing.T) {
	key := "org/repo#1"
	m := testAutoToggleModel(t, PR{
		Repo:   "org/repo",
		Number: 1,
	})
	m.autoRerun[key] = true
	m.lastAutoCheck["rerun:"+key] = time.Now()

	updated, _ := m.toggleAuto(m.autoRerun, m.rerunFailed, "autorerun")
	got := updated.(model)

	if got.autoRerun[key] {
		t.Fatal("autorerun was not disabled")
	}
	if !got.lastAutoCheck["rerun:"+key].IsZero() {
		t.Fatal("autorerun check cooldown was not cleared")
	}
}

func TestTruncateDisplayWidth(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{name: "ascii", in: "abcdef", max: 4, want: "abc…"},
		{name: "wide", in: "界界abc", max: 5, want: "界界…"},
		{name: "fits", in: "界a", max: 3, want: "界a"},
		{name: "tiny", in: "abcdef", max: 1, want: "…"},
		{name: "zero", in: "abcdef", max: 0, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := truncateDisplayWidth(tt.in, tt.max); got != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
		})
	}
}

func testAutoToggleModel(t *testing.T, pr PR) model {
	t.Helper()

	return model{
		config: Config{
			DataDir:        t.TempDir(),
			AutoCooldown:   time.Hour,
			AutoMaxRetries: 3,
			MergeComment:   "/m",
		},
		prs:           []PR{pr},
		autoMerge:     map[string]bool{},
		autoRerun:     map[string]bool{},
		lastAutoRerun: map[string]time.Time{},
		mergeFailed:   map[string]bool{},
		rerunFailed:   map[string]bool{},
		lastAutoCheck: map[string]time.Time{},
	}
}
