package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/utkuozdemir/gh-pr-lander/internal/config"
	"github.com/utkuozdemir/gh-pr-lander/internal/engine"
	"github.com/utkuozdemir/gh-pr-lander/internal/github"
	"github.com/utkuozdemir/gh-pr-lander/internal/pr"
)

// fakeClient is a no-op Client used to exercise the model without any network.
type fakeClient struct {
	username string
	mergeErr error
}

func (f fakeClient) FastForwardMerge(context.Context, string, string, string) error {
	return f.mergeErr
}

func (f fakeClient) FailedWorkflowRunsSummary(context.Context, string, string) (github.WorkflowSummary, error) {
	return github.WorkflowSummary{}, nil
}
func (f fakeClient) RerunFailedJobs(context.Context, string, string) error     { return nil }
func (f fakeClient) ApproveWorkflowRuns(context.Context, string, string) error { return nil }
func (f fakeClient) GetPRState(context.Context, string, int) (github.PRState, error) {
	return github.PRState{}, nil
}
func (f fakeClient) DeleteBranch(context.Context, string, string) error { return nil }
func (f fakeClient) Username() string                                   { return f.username }

func TestMergedCheckErrorDoesNotConfirmClosed(t *testing.T) {
	key := "org/repo#1"
	m := model{
		State: &engine.State{
			Client: fakeClient{},
			RecentlyMerged: map[string]*engine.MergedPR{
				key: {PR: pr.PR{Repo: "org/repo", Number: 1}, GoneAt: time.Now()},
			},
		},
	}

	updated, _ := m.Update(engine.MergedCheckResult{PRKey: key, Err: errors.New("boom")})
	got := updated.(model)

	if got.RecentlyMerged[key].Confirmed {
		t.Fatal("error result should not confirm merged/closed state")
	}
}

func TestMergedCheckConfirmsMergeWhenOnlyCleanupFailed(t *testing.T) {
	m := model{
		State: &engine.State{
			RecentlyMerged: map[string]*engine.MergedPR{"org/repo#1": {PR: pr.PR{Repo: "org/repo", Number: 1}}},
		},
	}

	updated, _ := m.Update(engine.MergedCheckResult{PRKey: "org/repo#1", Merged: true, Err: errors.New("422")})

	if mp := updated.(model).RecentlyMerged["org/repo#1"]; !mp.Confirmed || !mp.Merged {
		t.Fatalf("expected the merge to be confirmed, got %+v", mp)
	}
}

func TestMergedCheckOpenRemovesGoneEntry(t *testing.T) {
	key := "org/repo#1"
	m := model{
		State: &engine.State{
			Client: fakeClient{},
			RecentlyMerged: map[string]*engine.MergedPR{
				key: {PR: pr.PR{Repo: "org/repo", Number: 1}, GoneAt: time.Now()},
			},
		},
	}

	updated, _ := m.Update(engine.MergedCheckResult{PRKey: key, Open: true})
	got := updated.(model)

	if _, ok := got.RecentlyMerged[key]; ok {
		t.Fatal("open PR should be removed from recentlyMerged")
	}
}

func TestRenderMergedItemUsesClosedDescription(t *testing.T) {
	key := "org/repo#1"
	m := model{
		State: &engine.State{
			RecentlyMerged: map[string]*engine.MergedPR{
				key: {
					PR:        pr.PR{Repo: "org/repo", Number: 1, Title: "closed"},
					GoneAt:    time.Now().Add(-time.Minute),
					Confirmed: true,
					Merged:    false,
				},
			},
		},
		width: 80,
	}

	out := m.renderMergedItem(displayItem{
		pr:     pr.PR{Repo: "org/repo", Number: 1, Title: "closed"},
		merged: true,
		goneAt: m.RecentlyMerged[key].GoneAt,
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
		State: &engine.State{
			PRs: []pr.PR{{
				Repo:                    "org/repo",
				Number:                  1,
				CIStatus:                pr.CIFailed,
				FailedWorkflowRunsKnown: true,
				FailedWorkflowRuns:      0,
			}},
		},
	}

	updated, _ := m.doRerunChecks()
	got := updated.(model)

	if !strings.Contains(got.status, "No failed GitHub Actions jobs") {
		t.Fatalf("unexpected status: %q", got.status)
	}
}

func TestDoRerunChecksExplainsRunningWorkflow(t *testing.T) {
	m := model{
		State: &engine.State{
			PRs: []pr.PR{{
				Repo:                    "org/repo",
				Number:                  1,
				CIStatus:                pr.CIFailed,
				FailedWorkflowRunsKnown: true,
				FailedWorkflowRuns:      0,
				RunningWorkflowRuns:     1,
			}},
		},
	}

	updated, _ := m.doRerunChecks()
	got := updated.(model)

	if !strings.Contains(got.status, "A workflow is still running") {
		t.Fatalf("unexpected status: %q", got.status)
	}
}

func TestApproveWorkflowsOnlyForPRsAwaitingApproval(t *testing.T) {
	m := model{
		State: &engine.State{
			PRs: []pr.PR{{Repo: "org/repo", Number: 1}},
		},
	}

	if strings.Contains(m.renderFooter(), "approve workflows") {
		t.Fatal("footer offers approval on a PR with nothing awaiting it")
	}

	if _, cmd := m.doApproveWorkflows(); cmd != nil {
		t.Fatal("expected no approve command on a PR with nothing awaiting approval")
	}

	m.PRs[0].AwaitingApprovalRuns = 1

	if !strings.Contains(m.renderFooter(), "approve workflows") {
		t.Fatal("footer does not offer approval on a PR awaiting it")
	}

	if _, cmd := m.doApproveWorkflows(); cmd == nil {
		t.Fatal("expected an approve command")
	}
}

func TestAutoCmdsUsesPersistedAutoRerunCooldown(t *testing.T) {
	m := model{
		State: &engine.State{
			Config: config.Config{AutoCooldown: time.Hour},
			PRs: []pr.PR{{
				Repo:                    "org/repo",
				Number:                  1,
				CIStatus:                pr.CIFailed,
				FailedWorkflowRunsKnown: true,
				FailedWorkflowRuns:      1,
			}},
			AutoRerun:     map[string]bool{"org/repo#1": true},
			RerunFailed:   map[string]bool{},
			LastAutoCheck: map[string]time.Time{},
			LastAutoRerun: map[string]time.Time{"org/repo#1": time.Now()},
		},
	}

	if works := m.AutoWorks(); len(works) != 0 {
		t.Fatalf("expected cooldown to suppress autorerun, got %d works", len(works))
	}
}

func TestToggleAutoMergeChecksImmediatelyWhenReady(t *testing.T) {
	key := "org/repo#1"
	m := testAutoToggleModel(t, pr.PR{
		Repo:          "org/repo",
		Number:        1,
		OverallStatus: pr.StatusReady,
	})

	updated, _ := m.toggleAuto(engine.Automerge)
	got := updated.(model)

	if !got.AutoMerge[key] {
		t.Fatal("automerge was not enabled")
	}

	if got.LastAutoCheck["merge:"+key].IsZero() {
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

	updated, _ := m.toggleAuto(engine.Autorerun)
	got := updated.(model)

	if !got.AutoRerun[key] {
		t.Fatal("autorerun was not enabled")
	}

	if got.LastAutoCheck["rerun:"+key].IsZero() {
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
	m.LastAutoRerun[key] = time.Now()

	updated, _ := m.toggleAuto(engine.Autorerun)
	got := updated.(model)

	if !got.AutoRerun[key] {
		t.Fatal("autorerun was not enabled")
	}

	if !got.LastAutoCheck["rerun:"+key].IsZero() {
		t.Fatal("autorerun should not check immediately while persisted cooldown is active")
	}
}

func TestToggleAutoDisableClearsCheckCooldown(t *testing.T) {
	key := "org/repo#1"
	m := testAutoToggleModel(t, pr.PR{
		Repo:   "org/repo",
		Number: 1,
	})
	m.AutoRerun[key] = true
	m.LastAutoCheck["rerun:"+key] = time.Now()

	updated, _ := m.toggleAuto(engine.Autorerun)
	got := updated.(model)

	if got.AutoRerun[key] {
		t.Fatal("autorerun was not disabled")
	}

	if !got.LastAutoCheck["rerun:"+key].IsZero() {
		t.Fatal("autorerun check cooldown was not cleared")
	}
}

func TestRenderActivePRShowsReviewThreads(t *testing.T) {
	m := model{
		State: &engine.State{},
		width: 120,
	}

	for _, tc := range []struct {
		name             string
		resolved, total  int
		want, wantAbsent string
	}{
		{name: "unresolved", resolved: 4, total: 5, want: "Comments ◯ 4/5"},
		{name: "all resolved", resolved: 5, total: 5, want: "Comments ✓ 5/5"},
		{name: "no threads", wantAbsent: "Comments"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out := m.renderActivePR(pr.PR{
				Repo:                  "org/repo",
				Number:                1,
				ReviewThreads:         tc.total,
				ResolvedReviewThreads: tc.resolved,
			}, false)

			if tc.want != "" && !strings.Contains(out, tc.want) {
				t.Fatalf("expected %q in %q", tc.want, out)
			}

			if tc.wantAbsent != "" && strings.Contains(out, tc.wantAbsent) {
				t.Fatalf("did not expect %q in %q", tc.wantAbsent, out)
			}
		})
	}
}

func TestRenderActivePRShowsTakenOverAuthor(t *testing.T) {
	m := model{
		State: &engine.State{},
		width: 120,
	}

	out := m.renderActivePR(pr.PR{Repo: "org/repo", Number: 1, TakenOverFrom: "contributor"}, false)
	if !strings.Contains(out, "⇄ @contributor") {
		t.Fatalf("expected the original author in %q", out)
	}

	out = m.renderActivePR(pr.PR{Repo: "org/repo", Number: 1}, false)
	if strings.Contains(out, "⇄") {
		t.Fatalf("own PR should not show a takeover tag, got %q", out)
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
		State: &engine.State{
			Client: fakeClient{},
			Config: config.Config{
				DataDir:        t.TempDir(),
				PollInterval:   2 * time.Minute,
				AutoCooldown:   time.Hour,
				AutoMaxRetries: 3,
			},
			PRs:              []pr.PR{p},
			AutoMerge:        map[string]bool{},
			MinApprovals:     map[string]int{},
			ResolvedOnly:     map[string]bool{},
			AutoRerun:        map[string]bool{},
			LastAutoRerun:    map[string]time.Time{},
			MergeFailed:      map[string]bool{},
			RerunFailed:      map[string]bool{},
			LastAutoCheck:    map[string]time.Time{},
			RerunInFlight:    map[string]time.Time{},
			RerunInFlightSHA: map[string]string{},
		},
	}
}

func TestConditionKeysWorkBeforeAutomergeAndShowOnTheRow(t *testing.T) {
	p := pr.PR{Repo: "org/repo", Number: 1, Approvals: 1, ReviewThreads: 1}
	m := testAutoToggleModel(t, p)
	m.width = 120

	updated, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("2")})
	updated, _ = updated.(model).handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("c")})
	m = updated.(model)

	if row := m.renderActivePR(p, false); !strings.Contains(row, "automerge off (2 approvals, comments resolved)") {
		t.Fatalf("expected the row to show the conditions set ahead of automerge, got %q", row)
	}

	if !strings.Contains(m.status, "automerge off (2 approvals, comments resolved)") {
		t.Fatalf("expected the status to say automerge is off, got %q", m.status)
	}

	m.AutoMerge["org/repo#1"] = true

	if row := m.renderActivePR(
		p,
		false,
	); !strings.Contains(
		row,
		"automerge waits: 1 more approval, 1 unresolved comment",
	) {
		t.Fatalf("expected the row to say what automerge waits for, got %q", row)
	}

	p.Approvals, p.ResolvedReviewThreads = 2, 1

	if row := m.renderActivePR(p, false); !strings.Contains(row, "automerge (2 approvals, comments resolved)") {
		t.Fatalf("expected the row to show the conditions once they are met, got %q", row)
	}

	updated, _ = m.handleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("1")})
	if got := updated.(model); got.MinApprovals["org/repo#1"] != 0 {
		t.Fatalf("expected 1 to drop the approvals condition, got %d", got.MinApprovals["org/repo#1"])
	}
}

func TestFooterWrapsToTheTerminalWidth(t *testing.T) {
	m := model{State: &engine.State{PRs: []pr.PR{{Repo: "org/repo", Number: 1}}}, width: 60}

	footer := m.renderFooter()
	if !strings.Contains(footer, "quit") {
		t.Fatalf("expected the footer to keep every key, got %q", footer)
	}

	lines := strings.Split(footer, "\n")
	if len(lines) < 2 {
		t.Fatalf("expected the footer to wrap at 60 columns, got %q", footer)
	}

	for _, line := range lines {
		if width := lipgloss.Width(line); width > 60 {
			t.Fatalf("line is %d columns wide, wider than the terminal: %q", width, line)
		}
	}

	m.width = 400

	if strings.Contains(m.renderFooter(), "\n") {
		t.Fatal("expected one line on a terminal that is wide enough")
	}
}

func TestHeaderShowsTheRunningVersion(t *testing.T) {
	m := model{State: &engine.State{Config: config.Config{Version: "v1.2.3 (abc1234)"}}, width: 80}

	if !strings.Contains(m.renderHeader(), "v1.2.3 (abc1234)") {
		t.Fatalf("expected the header to show the version, got %q", m.renderHeader())
	}
}
