package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/utkuozdemir/sidero-rebaser/internal/config"
	"github.com/utkuozdemir/sidero-rebaser/internal/github"
	"github.com/utkuozdemir/sidero-rebaser/internal/pr"
)

// fakeClient is a no-op Client used to exercise the model without any network.
type fakeClient struct {
	username string
}

func (f fakeClient) PostComment(context.Context, string, int, string) (bool, error) {
	return false, nil
}
func (f fakeClient) PostCommentRaw(context.Context, string, int, string) error { return nil }
func (f fakeClient) CountMergeComments(context.Context, string, int, string) (int, time.Time, error) {
	return 0, time.Time{}, nil
}

func (f fakeClient) FailedWorkflowRunsSummary(context.Context, string, string) (github.WorkflowSummary, error) {
	return github.WorkflowSummary{}, nil
}
func (f fakeClient) RerunFailedJobs(context.Context, string, string) error { return nil }
func (f fakeClient) GetPRState(context.Context, string, int) (github.PRState, error) {
	return github.PRState{}, nil
}
func (f fakeClient) DeleteBranch(context.Context, string, string) error { return nil }
func (f fakeClient) Username() string                                   { return f.username }

func TestMergedCheckErrorDoesNotConfirmClosed(t *testing.T) {
	key := "org/repo#1"
	m := model{
		client: fakeClient{},
		recentlyMerged: map[string]*mergedPR{
			key: {PR: pr.PR{Repo: "org/repo", Number: 1}, GoneAt: time.Now()},
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
		client: fakeClient{},
		recentlyMerged: map[string]*mergedPR{
			key: {PR: pr.PR{Repo: "org/repo", Number: 1}, GoneAt: time.Now()},
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
			key: {
				PR:        pr.PR{Repo: "org/repo", Number: 1, Title: "closed"},
				GoneAt:    time.Now().Add(-time.Minute),
				Confirmed: true,
				Merged:    false,
			},
		},
	}

	out := m.renderMergedItem(displayItem{
		pr:     pr.PR{Repo: "org/repo", Number: 1, Title: "closed"},
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
		prs: []pr.PR{{
			Repo:                    "org/repo",
			Number:                  1,
			CIStatus:                pr.CIFailed,
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
		config: config.Config{AutoCooldown: time.Hour},
		prs: []pr.PR{{
			Repo:                    "org/repo",
			Number:                  1,
			CIStatus:                pr.CIFailed,
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
	m := testAutoToggleModel(t, pr.PR{
		Repo:          "org/repo",
		Number:        1,
		OverallStatus: pr.StatusReady,
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
	m := testAutoToggleModel(t, pr.PR{
		Repo:                    "org/repo",
		Number:                  1,
		CIStatus:                pr.CIFailed,
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
	m := testAutoToggleModel(t, pr.PR{
		Repo:                    "org/repo",
		Number:                  1,
		CIStatus:                pr.CIFailed,
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
	m := testAutoToggleModel(t, pr.PR{
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

func testAutoToggleModel(t *testing.T, p pr.PR) model {
	t.Helper()

	return model{
		client: fakeClient{},
		config: config.Config{
			DataDir:        t.TempDir(),
			PollInterval:   2 * time.Minute,
			AutoCooldown:   time.Hour,
			AutoMaxRetries: 3,
			MergeComment:   "/m",
		},
		prs:              []pr.PR{p},
		autoMerge:        map[string]bool{},
		autoRerun:        map[string]bool{},
		lastAutoRerun:    map[string]time.Time{},
		mergeFailed:      map[string]bool{},
		rerunFailed:      map[string]bool{},
		lastAutoCheck:    map[string]time.Time{},
		rerunInFlight:    map[string]time.Time{},
		rerunInFlightSHA: map[string]string{},
	}
}
