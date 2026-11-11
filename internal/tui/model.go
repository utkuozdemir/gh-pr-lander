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
	"sort"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/utkuozdemir/sidero-rebaser/internal/config"
	"github.com/utkuozdemir/sidero-rebaser/internal/git"
	"github.com/utkuozdemir/sidero-rebaser/internal/github"
	"github.com/utkuozdemir/sidero-rebaser/internal/notify"
	"github.com/utkuozdemir/sidero-rebaser/internal/pr"
	"github.com/utkuozdemir/sidero-rebaser/internal/store"
)

// mergedPR tracks a PR that disappeared from the open-PR search.
type mergedPR struct {
	PR        pr.PR
	GoneAt    time.Time
	Confirmed bool // API check completed
	Merged    bool // true = merged, false = closed without merge
}

// displayItem is one row in the combined active+merged list.
type displayItem struct {
	pr     pr.PR
	merged bool
	goneAt time.Time
}

type model struct {
	config  config.Config
	client  Client
	results <-chan github.FetchResult
	refresh func() // ask the poller to fetch immediately

	prs       []pr.PR
	prevPRs   []pr.PR
	cursor    int
	width     int
	height    int
	loading   bool
	err       error
	lastFetch time.Time
	status    string
	spinner   spinner.Model

	lastRerun      map[string]time.Time // manual rerun cooldown
	autoMerge      map[string]bool      // enabled flag, persisted in db.yaml
	autoRerun      map[string]bool      // enabled flag, persisted in db.yaml
	lastAutoRerun  map[string]time.Time // persisted auto-rerun cooldown
	mergeFailed    map[string]bool      // discovered from API, in-memory
	rerunFailed    map[string]bool      // discovered from API, in-memory
	lastAutoCheck  map[string]time.Time // cooldown gate to avoid API spam
	recentlyMerged map[string]*mergedPR // PRs that disappeared (merged/closed)

	autoRebase  map[string]int             // queue order, persisted in db.yaml
	rebaseState map[string]autoRebaseState // attempts and guards, in-memory
	rebasing    string                     // PR with a background rebase running

	// rerunInFlight guards against GitHub momentarily reporting "all checks
	// passed" right after a re-run is triggered (issue #1). While a PR is in
	// flight, a transient passed state is treated as still-running so automerge
	// does not fire a spurious merge. Keyed by PR; value is the trigger time and
	// the head SHA at that moment.
	rerunInFlight    map[string]time.Time
	rerunInFlightSHA map[string]string

	// checkBaseline guards against GitHub momentarily reporting an incomplete,
	// all-green check set right after an external rebase+push, before the new
	// commit's required checks have registered (issue #7). Keyed by PR.
	checkBaseline map[string]checkBaseline

	quitting bool
}

// checkBaseline remembers, per PR, the head SHA we last saw, when we first saw
// it, the high-water check count observed on it, and the high-water count on the
// SHA before it. A new commit whose current check count is below the previous
// commit's count is still settling.
type checkBaseline struct {
	sha          string
	firstSeen    time.Time
	maxTotal     int
	prevMaxTotal int
}

// NewModel builds the initial TUI model. results/refresh come from a
// github.Poller owned by the caller. It returns a tea.Model so callers do not
// depend on the unexported concrete type.
func NewModel(cfg config.Config, client Client, results <-chan github.FetchResult, refresh func()) tea.Model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(colorCyan)

	db, err := store.Load(cfg.DataDir)
	status := ""

	if err != nil {
		slog.Error("failed to load db", "error", err)
		status = errorStyle.Render("✗ Failed to load db.yaml: " + err.Error())
	}

	return model{
		config:           cfg,
		client:           client,
		results:          results,
		refresh:          refresh,
		loading:          true,
		status:           status,
		spinner:          s,
		lastRerun:        make(map[string]time.Time),
		autoMerge:        db.AutoMerge,
		autoRerun:        db.AutoRerun,
		lastAutoRerun:    db.AutoRerunLast,
		autoRebase:       db.AutoRebase,
		rebaseState:      make(map[string]autoRebaseState),
		mergeFailed:      make(map[string]bool),
		rerunFailed:      make(map[string]bool),
		lastAutoCheck:    make(map[string]time.Time),
		recentlyMerged:   make(map[string]*mergedPR),
		rerunInFlight:    make(map[string]time.Time),
		rerunInFlightSHA: make(map[string]string),
		checkBaseline:    make(map[string]checkBaseline),
	}
}

func (m model) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick,
		waitForResultsCmd(m.results),
	)
}

// triggerRefresh asks the poller to fetch now and shows the loading indicator.
func (m *model) triggerRefresh() {
	m.loading = true
	if m.refresh != nil {
		m.refresh()
	}
}

// displayItems returns active PRs followed by recently merged PRs as one list.
func (m model) displayItems() []displayItem {
	merged := m.sortedMerged()
	items := make([]displayItem, 0, len(m.prs)+len(merged))

	for _, p := range m.prs {
		items = append(items, displayItem{pr: p})
	}

	for _, mp := range merged {
		items = append(items, displayItem{pr: mp.PR, merged: true, goneAt: mp.GoneAt})
	}

	return items
}

func (m model) sortedMerged() []*mergedPR {
	now := time.Now()

	var result []*mergedPR

	for _, mp := range m.recentlyMerged {
		if now.Sub(mp.GoneAt) < m.config.MergedDisplayDuration {
			result = append(result, mp)
		}
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].GoneAt.After(result[j].GoneAt)
	})

	return result
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
		m.prevPRs = m.prs
		m.prs = m.applyChecksSettling(m.applyRerunSettling(msg.prs))
		m.lastFetch = time.Now()

		m.checkTransitions()
		deleteCmds := m.detectMerged()
		m = m.cleanupAuto()

		// Clamp cursor to combined list.
		items := m.displayItems()
		if m.cursor >= len(items) {
			m.cursor = max(0, len(items)-1)
		}

		cmds = append(cmds, m.autoCmds()...)
		cmds = append(cmds, m.autoRebaseCmd())
		cmds = append(cmds, deleteCmds...)

		return m, tea.Batch(cmds...)

	case mergeResultMsg:
		return m.handleMergeResult(msg)

	case checksRerunMsg:
		if msg.err != nil {
			slog.Error("failed to re-run checks", "pr", msg.pr.RepoShort(), "error", msg.err)
			m.status = errorStyle.Render("✗ Re-run failed: " + msg.err.Error())
		} else {
			slog.Info("re-running failed checks", "pr", msg.pr.RepoShort())
			m.status = successStyle.Render("✓ Re-running failed jobs on " + msg.pr.RepoShort())
			m.markRerunInFlight(msg.pr.RepoShort(), msg.pr.HeadSHA)
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

	case autoRerunResultMsg:
		return m.handleAutoRerunResult(msg)

	case autoRebaseResultMsg:
		return m.handleAutoRebaseResult(msg)

	case mergedCheckMsg:
		return m.handleMergedCheck(msg)

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
	items := m.displayItems()

	switch msg.String() {
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
		return m.toggleAuto(m.autoMerge, m.mergeFailed, "automerge")
	case "A":
		return m.toggleAuto(m.autoRerun, m.rerunFailed, "autorerun")
	case "b":
		return m.toggleAutoRebase()
	case "K":
		return m.moveInQueue(-1)
	case "J":
		return m.moveInQueue(1)
	case "o", "enter":
		return m.openInBrowser()
	case "R", "ctrl+r":
		m.status = ""
		m.triggerRefresh()

		return m, nil
	}

	return m, nil
}

// Actions — all check for merged PRs and bail if so (except open)

func (m model) doMerge() (tea.Model, tea.Cmd) {
	item, ok := m.selectedItem()
	if !ok || item.merged {
		return m, nil
	}

	if item.pr.OverallStatus != pr.StatusReady {
		m.status = errorStyle.Render("✗ PR not ready to merge — need synced base + passing CI")

		return m, clearStatusCmd()
	}

	m.status = fmt.Sprintf("Merging %s...", item.pr.RepoShort())

	return m, mergeCmd(m.client, item.pr, false)
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

	script, err := git.BuildRebaseScript(m.config.DataDir, item.pr, m.config.RebaseCountdown)
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

	if item.pr.CIStatus != pr.CIFailed {
		m.status = errorStyle.Render("✗ No failed checks to re-run")

		return m, clearStatusCmd()
	}

	if item.pr.FailedWorkflowRunsKnown && item.pr.FailedWorkflowRuns == 0 {
		if item.pr.RunningWorkflowRuns > 0 {
			m.status = indicatorWarn.Render(
				"⚠ A workflow is still running. If the failure is in it, failed jobs can be re-run " +
					"once it finishes (or press A for autorerun)",
			)

			return m, clearStatusCmd()
		}

		m.status = errorStyle.Render("✗ No failed GitHub Actions jobs to re-run")

		return m, clearStatusCmd()
	}

	key := item.pr.RepoShort()
	if last, ok := m.lastRerun[key]; ok && time.Since(last) < time.Minute {
		m.status = indicatorWarn.Render("⚠ Already re-ran checks less than a minute ago")

		return m, clearStatusCmd()
	}

	m.lastRerun[key] = time.Now()
	m.status = fmt.Sprintf("Re-running failed jobs on %s...", key)

	return m, rerunChecksCmd(m.client, item.pr)
}

func (m model) doApproveWorkflows() (tea.Model, tea.Cmd) {
	item, ok := m.selectedItem()
	if !ok || item.merged || item.pr.AwaitingApprovalRuns == 0 {
		return m, nil
	}

	m.status = fmt.Sprintf("Approving workflows on %s...", item.pr.RepoShort())

	return m, approveWorkflowsCmd(m.client, item.pr)
}

func (m model) openInBrowser() (tea.Model, tea.Cmd) {
	item, ok := m.selectedItem()
	if !ok {
		return m, nil
	}
	// Works on both active and merged PRs. The URL comes from the GitHub API.
	_ = exec.CommandContext(context.Background(), "open", item.pr.URL).Start()

	return m, nil
}

func (m model) toggleAuto(entries map[string]bool, failedMap map[string]bool, label string) (tea.Model, tea.Cmd) {
	item, ok := m.selectedItem()
	if !ok || item.merged {
		return m, nil
	}

	key := item.pr.RepoShort()
	enabled := false

	if entries[key] {
		delete(entries, key)
		delete(failedMap, key)
		m.clearAutoCheck(label, key)

		if label == "autorerun" {
			delete(m.lastAutoRerun, key)
		}

		slog.Info(label+" disabled", "pr", key)
		m.status = indicatorAuto.Render("⚡ " + label + " disabled for " + key)
	} else {
		entries[key] = true
		enabled = true

		slog.Info(label+" enabled", "pr", key)
		m.status = indicatorAuto.Render("⚡ " + label + " enabled for " + key)
	}

	if err := m.saveDB(); err != nil {
		slog.Error("failed to save db", "error", err)
		m.status = errorStyle.Render("✗ Failed to save db.yaml: " + err.Error())

		return m, clearStatusCmd()
	}

	if enabled {
		if cmd := m.immediateAutoCmd(item.pr, label); cmd != nil {
			m.status = indicatorAuto.Render("⚡ " + label + " enabled for " + key + "; checking now")

			return m, tea.Batch(clearStatusCmd(), cmd)
		}
	}

	return m, clearStatusCmd()
}

func (m model) saveDB() error {
	return store.Save(m.config.DataDir, store.DB{
		AutoMerge:     m.autoMerge,
		AutoRerun:     m.autoRerun,
		AutoRerunLast: m.lastAutoRerun,
		AutoRebase:    m.autoRebase,
	})
}

func (m model) handleMergeResult(msg mergeResultMsg) (tea.Model, tea.Cmd) {
	key := msg.pr.RepoShort()

	if msg.err != nil {
		slog.Error("failed to merge", "pr", key, "auto", msg.auto, "error", msg.err)
		m.status = errorStyle.Render("✗ Failed to merge " + key + ": " + msg.err.Error())

		// GitHub refused this commit, and asking again would get the same
		// answer. Automerge tries again when the PR gets a new head commit.
		// Any other error says nothing about the commit: a base branch that
		// moved shows the PR as behind on the next fetch, and after a network
		// or server error automerge tries again once the cooldown is over.
		if msg.auto && errors.Is(msg.err, github.ErrMergeRefused) {
			m.mergeFailed[key] = true
			notify.Send(notify.AutomergeGaveUp, key+": "+msg.err.Error())
		}
	} else {
		slog.Info("merged", "pr", key, "auto", msg.auto)
		m.status = successStyle.Render("✓ Merged " + key)
	}

	m.triggerRefresh()

	return m, tea.Batch(
		clearStatusCmd(),
		delayedRefreshCmd(15*time.Second),
		delayedRefreshCmd(30*time.Second),
	)
}

func (m model) handleAutoRerunResult(msg autoRerunResultMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.failed:
		slog.Error("autorerun gave up", "pr", msg.prKey)
		m.rerunFailed[msg.prKey] = true
		m.status = errorStyle.Render(
			fmt.Sprintf("⚡ %s: re-ran %d times, still failing — giving up", msg.prKey, m.config.AutoMaxRetries),
		)
		notify.Send(notify.AutorerunGaveUp, msg.prKey+" — still failing")
	case msg.reran:
		slog.Info("autorerun triggered", "pr", msg.prKey)

		if msg.reranAt.IsZero() {
			msg.reranAt = time.Now()
		}

		m.lastAutoRerun[msg.prKey] = msg.reranAt
		m.markRerunInFlight(msg.prKey, msg.headSHA)

		if err := m.saveDB(); err != nil {
			slog.Error("failed to save db", "error", err)
			m.status = errorStyle.Render("⚡ Autorerun triggered, but failed to save db.yaml: " + err.Error())

			return m, clearStatusCmd()
		}

		m.status = successStyle.Render("⚡ Autorerun: re-running failed jobs on " + msg.prKey)
		m.triggerRefresh()

		return m, tea.Batch(clearStatusCmd(), delayedRefreshCmd(15*time.Second))
	case msg.unavailable:
		slog.Info("autorerun skipped, no failed GitHub Actions runs", "pr", msg.prKey)
	case msg.err != nil:
		slog.Error("autorerun error", "pr", msg.prKey, "error", msg.err)
	}

	return m, clearStatusCmd()
}

// confirmGone records the API-confirmed outcome of a PR that left the list and
// notifies about it.
func (m model) confirmGone(key string, merged bool) {
	mp := m.recentlyMerged[key]
	if mp == nil {
		return
	}

	mp.Confirmed = true
	mp.Merged = merged

	event := notify.Closed
	if merged {
		event = notify.Merged
	}

	notify.Send(event, key+" — "+mp.PR.Title)
}

func (m model) handleMergedCheck(msg mergedCheckMsg) (tea.Model, tea.Cmd) {
	switch {
	case msg.err != nil:
		slog.Error("merged PR check/cleanup error", "pr", msg.prKey, "error", msg.err)

		// Only the branch cleanup failed, the merge itself is confirmed.
		if msg.merged {
			m.confirmGone(msg.prKey, true)
		}
	case msg.open:
		delete(m.recentlyMerged, msg.prKey)
		slog.Info("PR is still open, keeping it out of merged/closed list", "pr", msg.prKey)
	case msg.deleted:
		m.confirmGone(msg.prKey, true)
		slog.Info("deleted merged branch", "pr", msg.prKey, "branch", msg.branch)
	case !msg.merged:
		m.confirmGone(msg.prKey, false)
		slog.Info("PR closed without merge", "pr", msg.prKey)
	default:
		m.confirmGone(msg.prKey, true)
	}

	return m, nil
}
