package tui

import (
	"context"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/utkuozdemir/gh-pr-lander/internal/engine"
	"github.com/utkuozdemir/gh-pr-lander/internal/github"
	"github.com/utkuozdemir/gh-pr-lander/internal/pr"
)

// Messages

// resultsMsg carries one poll outcome from the background poller.
type resultsMsg struct {
	prs []pr.PR
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

// workCmd runs engine work in the background. Its result comes back as a
// message.
func workCmd(w engine.Work) tea.Cmd {
	if w == nil {
		return nil
	}

	return func() tea.Msg { return w() }
}

func workCmds(works []engine.Work) []tea.Cmd {
	cmds := make([]tea.Cmd, 0, len(works))
	for _, w := range works {
		cmds = append(cmds, workCmd(w))
	}

	return cmds
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
