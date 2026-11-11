package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/utkuozdemir/sidero-rebaser/internal/github"
	"github.com/utkuozdemir/sidero-rebaser/internal/pr"
)

// Messages

// resultsMsg carries one poll outcome from the background poller.
type resultsMsg struct {
	prs []pr.PR
	err error
}

// mergeResultMsg is the outcome of a fast-forward merge, started by the user
// or by automerge (auto).
type mergeResultMsg struct {
	pr   pr.PR
	auto bool
	err  error
}

type checksRerunMsg struct {
	pr  pr.PR
	err error
}

type workflowsApprovedMsg struct {
	pr  pr.PR
	err error
}

type rebaseFinishedMsg struct {
	err error
}

type clearStatusMsg struct{}

type delayedRefreshMsg struct{}

type mergedCheckMsg struct {
	prKey   string
	branch  string
	open    bool // true if the PR was omitted from search but is still open
	merged  bool // true if API confirmed merged, false if closed without merge
	deleted bool // true if branch was actually deleted
	err     error
}

type autoRerunResultMsg struct {
	prKey       string
	headSHA     string
	reran       bool
	failed      bool // max retries exceeded
	unavailable bool // no failed GitHub Actions runs exist for this SHA
	reranAt     time.Time
	err         error
}

// Commands

// waitForResultsCmd blocks on the poller's result stream and yields one
// resultsMsg. The Update loop re-issues it after each result, turning the
// channel into a continuous source of UI updates.
func waitForResultsCmd(results <-chan github.FetchResult) tea.Cmd {
	return func() tea.Msg {
		res, ok := <-results
		if !ok {
			return nil
		}

		return resultsMsg{prs: res.PRs, err: res.Err}
	}
}

func delayedRefreshCmd(delay time.Duration) tea.Cmd {
	return tea.Tick(delay, func(_ time.Time) tea.Msg {
		return delayedRefreshMsg{}
	})
}

func mergeCmd(client Client, p pr.PR, auto bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		err := client.FastForwardMerge(ctx, p.Repo, p.BaseBranch, p.HeadSHA)

		return mergeResultMsg{pr: p, auto: auto, err: err}
	}
}

func rerunChecksCmd(client Client, p pr.PR) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		err := client.RerunFailedJobs(ctx, p.Repo, p.HeadSHA)

		return checksRerunMsg{pr: p, err: err}
	}
}

func approveWorkflowsCmd(client Client, p pr.PR) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		err := client.ApproveWorkflowRuns(ctx, p.Repo, p.HeadSHA)

		return workflowsApprovedMsg{pr: p, err: err}
	}
}

func clearStatusCmd() tea.Cmd {
	return tea.Tick(5*time.Second, func(_ time.Time) tea.Msg {
		return clearStatusMsg{}
	})
}

// checkMergedPRCmd verifies whether a PR was merged, and optionally deletes the
// head branch.
func checkMergedPRCmd(client Client, p pr.PR, deleteBranch bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		key := p.RepoShort()

		state, err := client.GetPRState(ctx, p.Repo, p.Number)
		if err != nil {
			return mergedCheckMsg{prKey: key, branch: p.HeadBranch, err: fmt.Errorf("check merged: %w", err)}
		}

		if strings.EqualFold(state.State, "open") {
			return mergedCheckMsg{prKey: key, branch: p.HeadBranch, open: true}
		}

		if !state.Merged || !deleteBranch {
			return mergedCheckMsg{prKey: key, branch: p.HeadBranch, merged: state.Merged}
		}

		if err := client.DeleteBranch(ctx, p.HeadRepo, p.HeadBranch); err != nil {
			return mergedCheckMsg{prKey: key, branch: p.HeadBranch, merged: true, err: err}
		}

		return mergedCheckMsg{prKey: key, branch: p.HeadBranch, merged: true, deleted: true}
	}
}

// autoRerunCmd checks run_attempt from the GitHub API, then reruns if
// appropriate.
func autoRerunCmd(client Client, p pr.PR, maxRetries int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		key := p.RepoShort()

		summary, err := client.FailedWorkflowRunsSummary(ctx, p.Repo, p.HeadSHA)
		if err != nil {
			return autoRerunResultMsg{prKey: key, headSHA: p.HeadSHA, err: err}
		}

		if summary.Count == 0 {
			return autoRerunResultMsg{prKey: key, headSHA: p.HeadSHA, unavailable: true}
		}

		// run_attempt=1 is the original run, so run_attempt > maxRetries means
		// we've rerun maxRetries times.
		if summary.MaxAttempt > maxRetries {
			return autoRerunResultMsg{prKey: key, headSHA: p.HeadSHA, failed: true}
		}

		if err := client.RerunFailedJobs(ctx, p.Repo, p.HeadSHA); err != nil {
			return autoRerunResultMsg{prKey: key, headSHA: p.HeadSHA, err: err}
		}

		return autoRerunResultMsg{prKey: key, headSHA: p.HeadSHA, reran: true, reranAt: time.Now()}
	}
}
