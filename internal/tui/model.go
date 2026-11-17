// Package tui is the Bubble Tea front end: the model, the update loop, and the
// rendering. It depends on the GitHub API only through the narrow Client
// interface (see client.go) and receives PR updates from a background poller
// over a channel, so the UI stays responsive and the logic stays testable.
package tui

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/utkuozdemir/gh-pr-lander/internal/config"
	"github.com/utkuozdemir/gh-pr-lander/internal/engine"
	"github.com/utkuozdemir/gh-pr-lander/internal/git"
	"github.com/utkuozdemir/gh-pr-lander/internal/github"
	"github.com/utkuozdemir/gh-pr-lander/internal/pr"
)

// displayItem is one row in the combined active+merged list.
type displayItem struct {
	pr     pr.PR
	merged bool
	goneAt time.Time
}

type model struct {
	// The pull requests, the automation switches and everything else that
	// needs no terminal live in the engine, shared with the server.
	*engine.State

	results <-chan github.FetchResult
	refresh func() // ask the poller to fetch immediately

	cursor    int
	width     int
	height    int
	loading   bool
	err       error
	lastFetch time.Time
	status    string
	spinner   spinner.Model

	showHidden bool // the hidden PRs are listed too, not saved

	help       bool // the help view is shown instead of the list
	helpOffset int  // how far the help is scrolled down

	rebaseState map[string]autoRebaseState // attempts and guards, in-memory
	rebasing    string                     // PR with a background rebase running

	quitting bool
}

// NewModel builds the initial TUI model. results/refresh come from a
// github.Poller owned by the caller. It returns a tea.Model so callers do not
// depend on the unexported concrete type.
func NewModel(cfg config.Config, client Client, results <-chan github.FetchResult, refresh func()) tea.Model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(colorCyan)

	state, err := engine.New(cfg, client)
	status := ""

	if err != nil {
		slog.Error("failed to load db", "error", err)
		status = errorStyle.Render("✗ Failed to load db.yaml: " + err.Error())
	}

	return model{
		State:       state,
		results:     results,
		refresh:     refresh,
		loading:     true,
		status:      status,
		spinner:     s,
		rebaseState: make(map[string]autoRebaseState),
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick,
		waitForResultsCmd(m.results),
		ageTickCmd(),
	)
}

// triggerRefresh asks the poller to fetch now and shows the loading indicator.
func (m *model) triggerRefresh() {
	m.loading = true
	if m.refresh != nil {
		m.refresh()
	}
}

// displayItems returns the open PRs, then the hidden ones when they are being
// shown, then the recently merged ones, as one list.
func (m model) displayItems() []displayItem {
	merged := m.SortedMerged()
	listed, hidden := m.Shown()
	items := make([]displayItem, 0, len(m.PRs)+len(merged))

	for _, p := range listed {
		items = append(items, displayItem{pr: p})
	}

	if m.showHidden {
		for _, p := range hidden {
			items = append(items, displayItem{pr: p})
		}
	}

	for _, mp := range merged {
		items = append(items, displayItem{pr: mp.PR, merged: true, goneAt: mp.GoneAt})
	}

	return items
}

func (m model) selectedItem() (displayItem, bool) {
	items := m.displayItems()
	if m.cursor >= 0 && m.cursor < len(items) {
		return items[m.cursor], true
	}

	return displayItem{}, false
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height

		return m, nil

	case spinner.TickMsg:
		var cmd tea.Cmd

		m.spinner, cmd = m.spinner.Update(msg)

		return m, cmd

	case ageTickMsg:
		// Nothing changes but the time. Returning redraws the view, and with it
		// every "54s ago".
		return m, ageTickCmd()

	case delayedRefreshMsg:
		m.triggerRefresh()

		return m, nil

	case resultsMsg:
		// Keep listening for the next poll result.
		cmds := make([]tea.Cmd, 0, 1)
		cmds = append(cmds, waitForResultsCmd(m.results))

		m.loading = false

		if msg.err != nil {
			slog.Error("failed to fetch PRs", "error", msg.err)
			m.err = msg.err

			return m, tea.Batch(cmds...)
		}

		m.err = nil
		m.lastFetch = time.Now()

		mergedChecks, err := m.Apply(msg.prs)
		if err != nil {
			m.status = errorStyle.Render("✗ Failed to save db.yaml: " + err.Error())
		}

		// The engine drops the queue entry of a PR that is gone.
		for key := range m.rebaseState {
			if _, queued := m.AutoRebase[key]; !queued {
				delete(m.rebaseState, key)
			}
		}

		// Clamp cursor to combined list.
		items := m.displayItems()
		if m.cursor >= len(items) {
			m.cursor = max(0, len(items)-1)
		}

		cmds = append(cmds, workCmds(m.AutoWorks())...)
		cmds = append(cmds, m.autoRebaseCmd())
		cmds = append(cmds, workCmds(mergedChecks)...)

		return m, tea.Batch(cmds...)

	case engine.MergeResult:
		return m.handleMergeResult(msg)

	case engine.RerunResult:
		m.RerunDone(msg)

		if msg.Err != nil {
			m.status = errorStyle.Render("✗ Re-run failed: " + msg.Err.Error())
		} else {
			m.status = successStyle.Render("✓ Re-running failed jobs on " + msg.PR.RepoShort())
		}

		m.triggerRefresh()

		return m, tea.Batch(clearStatusCmd(), delayedRefreshCmd(15*time.Second))

	case workflowsApprovedMsg:
		if msg.err != nil {
			slog.Error("failed to approve workflows", "pr", msg.pr.RepoShort(), "error", msg.err)
			m.status = errorStyle.Render("✗ Approve failed: " + msg.err.Error())
		} else {
			slog.Info("approved workflows", "pr", msg.pr.RepoShort())
			m.status = successStyle.Render("✓ Approved workflows on " + msg.pr.RepoShort())
		}

		m.triggerRefresh()

		return m, tea.Batch(clearStatusCmd(), delayedRefreshCmd(15*time.Second))

	case rebaseFinishedMsg:
		m.rebasing = ""

		if msg.err != nil {
			slog.Error("rebase failed", "error", msg.err)
			m.status = errorStyle.Render("✗ Rebase failed: " + msg.err.Error())
		} else {
			slog.Info("rebase completed successfully")

			m.status = successStyle.Render("✓ Rebase complete!")
		}

		m.triggerRefresh()

		return m, tea.Batch(clearStatusCmd(), delayedRefreshCmd(15*time.Second))

	case engine.AutoRerunResult:
		return m.handleAutoRerunResult(msg)

	case autoRebaseResultMsg:
		return m.handleAutoRebaseResult(msg)

	case engine.MergedCheckResult:
		m.MergedCheckDone(msg)

		return m, nil

	case clearStatusMsg:
		m.status = ""

		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	return m, nil
}

// Key handling

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if m.help {
		return m.handleHelpKey(msg)
	}

	items := m.displayItems()

	switch msg.String() {
	case "?":
		m.help = true
		m.helpOffset = 0

		return m, nil
	case "q", "ctrl+c":
		m.quitting = true

		return m, tea.Quit

	case "j", "down":
		if m.cursor < len(items)-1 {
			m.cursor++
		}

		return m, nil

	case "k", "up":
		if m.cursor > 0 {
			m.cursor--
		}

		return m, nil

	case "g", "home":
		m.cursor = 0

		return m, nil

	case "G", "end":
		m.cursor = max(0, len(items)-1)

		return m, nil

	case "m":
		return m.doMerge()
	case "r":
		return m.doRebase()
	case "t":
		return m.doRerunChecks()
	case "w":
		return m.doApproveWorkflows()
	case "a":
		return m.toggleAuto(engine.Automerge)
	case "A":
		return m.toggleAuto(engine.Autorerun)
	case "1", "2", "3":
		return m.setCondition(func(p pr.PR) (engine.Work, error) {
			return m.SetMinApprovals(p, int(msg.String()[0]-'0'))
		})
	case "c":
		return m.setCondition(m.ToggleResolvedOnly)
	case "b":
		return m.toggleAutoRebase()
	case "K":
		return m.moveInQueue(-1)
	case "J":
		return m.moveInQueue(1)
	case "o", "enter":
		return m.openInBrowser(func(p pr.PR) string { return p.URL })
	case "O":
		return m.openInBrowser(func(p pr.PR) string { return p.ChecksURL })
	case "y":
		return m.copyURL()
	case "h":
		return m.toggleHidden()
	case "H":
		m.showHidden = !m.showHidden
		m.cursor = min(m.cursor, max(0, len(m.displayItems())-1))

		return m, nil
	case "R", "ctrl+r":
		m.status = ""
		m.triggerRefresh()

		return m, nil
	}

	return m, nil
}

// handleHelpKey handles a key while the help view covers the list. The list
// keys scroll it, and any other key closes it without doing what it normally
// does.
func (m model) handleHelpKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		m.quitting = true

		return m, tea.Quit
	case "j", "down":
		m.helpOffset = min(m.helpOffset+1, m.helpMaxOffset())
	case "k", "up":
		m.helpOffset = max(m.helpOffset-1, 0)
	case "g", "home":
		m.helpOffset = 0
	case "G", "end":
		m.helpOffset = m.helpMaxOffset()
	default:
		m.help = false
	}

	return m, nil
}

// Actions — all check for merged PRs and bail if so (except open)

func (m model) doMerge() (tea.Model, tea.Cmd) {
	item, ok := m.selectedItem()
	if !ok || item.merged {
		return m, nil
	}

	if err := m.CheckMerge(item.pr); err != nil {
		m.status = refusalStatus(err)

		return m, clearStatusCmd()
	}

	m.status = fmt.Sprintf("Merging %s...", item.pr.RepoShort())

	return m, workCmd(m.MergeWork(item.pr, false))
}

func (m model) doRebase() (tea.Model, tea.Cmd) {
	item, ok := m.selectedItem()
	if !ok || item.merged {
		return m, nil
	}

	if item.pr.BaseStatus == pr.BaseConflict {
		m.status = errorStyle.Render("✗ PR has conflicts — resolve manually")

		return m, clearStatusCmd()
	}

	// Both would use the same clone under the data dir.
	if m.rebasing != "" {
		m.status = indicatorWarn.Render("⚠ Autorebase of " + m.rebasing + " is running, try again when it finishes")

		return m, clearStatusCmd()
	}

	script, err := git.BuildRebaseScript(m.Config.DataDir, item.pr, m.Config.RebaseCountdown)
	if err != nil {
		m.status = errorStyle.Render("✗ " + err.Error())

		return m, clearStatusCmd()
	}

	// Run interactively in the foreground via tea.ExecProcess so the user can
	// answer YubiKey prompts. The script is built internally from API data.
	// Reserve the clone right away: the exec runs only once Bubble Tea gets to
	// it, and a poll result handled before that must not start an autorebase.
	cmd := exec.CommandContext(context.Background(), "bash", "-c", script)
	m.rebasing = item.pr.RepoShort()

	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		return rebaseFinishedMsg{err: err}
	})
}

func (m model) doRerunChecks() (tea.Model, tea.Cmd) {
	item, ok := m.selectedItem()
	if !ok || item.merged {
		return m, nil
	}

	if err := m.StartRerun(item.pr); err != nil {
		m.status = refusalStatus(err)

		return m, clearStatusCmd()
	}

	m.status = fmt.Sprintf("Re-running failed jobs on %s...", item.pr.RepoShort())

	return m, workCmd(m.RerunWork(item.pr))
}

// refusalStatus renders why the engine did not start an action.
func refusalStatus(err error) string {
	var refusal *engine.RefusedError
	if errors.As(err, &refusal) && refusal.Soft {
		return indicatorWarn.Render("⚠ " + refusal.Reason)
	}

	return errorStyle.Render("✗ " + err.Error())
}

func (m model) doApproveWorkflows() (tea.Model, tea.Cmd) {
	item, ok := m.selectedItem()
	if !ok || item.merged || item.pr.AwaitingApprovalRuns == 0 {
		return m, nil
	}

	m.status = fmt.Sprintf("Approving workflows on %s...", item.pr.RepoShort())

	return m, approveWorkflowsCmd(m.Client, item.pr)
}

// openInBrowser opens a page of the selected PR: the PR itself or its checks.
func (m model) openInBrowser(page func(pr.PR) string) (tea.Model, tea.Cmd) {
	item, ok := m.selectedItem()
	if !ok {
		return m, nil
	}
	// Works on both active and merged PRs. The URL comes from the GitHub API.
	_ = exec.CommandContext(context.Background(), "open", page(item.pr)).Start()

	return m, nil
}

// copyURL puts the selected PR's URL on the clipboard. It works on merged PRs
// too.
func (m model) copyURL() (tea.Model, tea.Cmd) {
	item, ok := m.selectedItem()
	if !ok {
		return m, nil
	}

	cmd := exec.CommandContext(context.Background(), "pbcopy")
	cmd.Stdin = strings.NewReader(item.pr.URL)

	if err := cmd.Run(); err != nil {
		m.status = errorStyle.Render("✗ Failed to copy: " + err.Error())
	} else {
		m.status = successStyle.Render("✓ Copied " + item.pr.URL)
	}

	return m, clearStatusCmd()
}

// toggleHidden hides the selected PR, or shows it again when it is hidden.
func (m model) toggleHidden() (tea.Model, tea.Cmd) {
	item, ok := m.selectedItem()
	if !ok || item.merged {
		return m, nil
	}

	key := item.pr.RepoShort()

	hidden, err := m.ToggleHidden(item.pr)

	switch {
	case err != nil:
		m.status = errorStyle.Render("✗ Failed to save db.yaml: " + err.Error())
	case hidden:
		m.status = indicatorNone.Render("Hid " + key + ". H shows the hidden ones")
	default:
		m.status = indicatorNone.Render(key + " is back in the list")
	}

	// The row left the list, or moved within it.
	m.cursor = min(m.cursor, max(0, len(m.displayItems())-1))

	return m, clearStatusCmd()
}

func (m model) toggleAuto(kind engine.Kind) (tea.Model, tea.Cmd) {
	item, ok := m.selectedItem()
	if !ok || item.merged {
		return m, nil
	}

	key := item.pr.RepoShort()
	label := string(kind)

	enabled, work, err := m.Toggle(kind, item.pr)
	if err != nil {
		m.status = errorStyle.Render("✗ Failed to save db.yaml: " + err.Error())

		return m, clearStatusCmd()
	}

	switch {
	case !enabled:
		m.status = indicatorAuto.Render("⚡ " + label + " disabled for " + key)
	case work != nil:
		m.status = indicatorAuto.Render("⚡ " + label + " enabled for " + key + "; checking now")
	default:
		m.status = indicatorAuto.Render("⚡ " + label + " enabled for " + key)
	}

	return m, tea.Batch(clearStatusCmd(), workCmd(work))
}

// setCondition changes one automerge condition of the selected PR. It works
// with automerge off too, so the conditions can be in place before it goes on.
func (m model) setCondition(set func(pr.PR) (engine.Work, error)) (tea.Model, tea.Cmd) {
	item, ok := m.selectedItem()
	if !ok || item.merged {
		return m, nil
	}

	key := item.pr.RepoShort()

	work, err := set(item.pr)
	if err != nil {
		m.status = errorStyle.Render("✗ Failed to save db.yaml: " + err.Error())

		return m, clearStatusCmd()
	}

	m.status = indicatorNone.Render(key + ": " + m.automergeText(item.pr))
	if m.AutoMerge[key] {
		m.status = indicatorAuto.Render("⚡ " + key + ": " + m.automergeText(item.pr))
	}

	return m, tea.Batch(clearStatusCmd(), workCmd(work))
}

func (m model) handleMergeResult(msg engine.MergeResult) (tea.Model, tea.Cmd) {
	key := msg.PR.RepoShort()

	m.MergeDone(msg)

	if msg.Err != nil {
		m.status = errorStyle.Render("✗ Failed to merge " + key + ": " + msg.Err.Error())
	} else {
		m.status = successStyle.Render("✓ Merged " + key)
	}

	m.triggerRefresh()

	return m, tea.Batch(
		clearStatusCmd(),
		delayedRefreshCmd(15*time.Second),
		delayedRefreshCmd(30*time.Second),
	)
}

func (m model) handleAutoRerunResult(msg engine.AutoRerunResult) (tea.Model, tea.Cmd) {
	saveErr := m.AutoRerunDone(msg)

	switch {
	case msg.Failed:
		m.status = errorStyle.Render(
			fmt.Sprintf("⚡ %s: re-ran %d times, still failing — giving up", msg.PRKey, m.Config.AutoMaxRetries),
		)
	case msg.Reran:
		if saveErr != nil {
			m.status = errorStyle.Render("⚡ Autorerun triggered, but failed to save db.yaml: " + saveErr.Error())

			return m, clearStatusCmd()
		}

		m.status = successStyle.Render("⚡ Autorerun: re-running failed jobs on " + msg.PRKey)
		m.triggerRefresh()

		return m, tea.Batch(clearStatusCmd(), delayedRefreshCmd(15*time.Second))
	}

	return m, clearStatusCmd()
}
