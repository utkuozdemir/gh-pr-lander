package main

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Messages

type tickMsg struct{}

type prsLoadedMsg struct {
	requestID int
	prs       []PR
	err       error
}

type commentPostedMsg struct {
	pr        PR
	err       error
	duplicate bool
}

type checksRerunMsg struct {
	pr  PR
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

// Model

type mergedPR struct {
	PR        PR
	GoneAt    time.Time
	Confirmed bool // API check completed
	Merged    bool // true = merged, false = closed without merge
}

type displayItem struct {
	pr     PR
	merged bool
	goneAt time.Time
}

type model struct {
	config         Config
	client         *GitHubClient
	prs            []PR
	prevPRs        []PR
	cursor         int
	width          int
	height         int
	loading        bool
	err            error
	lastFetch      time.Time
	status         string
	spinner        spinner.Model
	lastRerun      map[string]time.Time // manual rerun cooldown
	autoMerge      map[string]bool      // enabled flag, persisted in db.yaml
	autoRerun      map[string]bool      // enabled flag, persisted in db.yaml
	lastAutoRerun  map[string]time.Time // persisted auto-rerun cooldown
	mergeFailed    map[string]bool      // discovered from API, in-memory
	rerunFailed    map[string]bool      // discovered from API, in-memory
	lastAutoCheck  map[string]time.Time // cooldown gate to avoid API spam
	recentlyMerged map[string]*mergedPR // PRs that disappeared (merged/closed)
	nextFetchID    int
	latestFetchID  int
	quitting       bool
}

func newModel(cfg Config, client *GitHubClient) model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(colorCyan)

	am, ar, lastAutoRerun, err := loadDB(cfg.DataDir)
	status := ""
	if err != nil {
		slog.Error("failed to load db", "error", err)
		status = errorStyle.Render("✗ Failed to load db.yaml: " + err.Error())
	}

	return model{
		config:         cfg,
		client:         client,
		loading:        true,
		status:         status,
		spinner:        s,
		lastRerun:      make(map[string]time.Time),
		autoMerge:      am,
		autoRerun:      ar,
		lastAutoRerun:  lastAutoRerun,
		mergeFailed:    make(map[string]bool),
		rerunFailed:    make(map[string]bool),
		lastAutoCheck:  make(map[string]time.Time),
		recentlyMerged: make(map[string]*mergedPR),
		nextFetchID:    2,
		latestFetchID:  1,
	}
}

// displayItems returns active PRs followed by recently merged PRs as one list.
func (m model) displayItems() []displayItem {
	var items []displayItem
	for _, pr := range m.prs {
		items = append(items, displayItem{pr: pr})
	}
	for _, mp := range m.sortedMerged() {
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

func (m model) Init() tea.Cmd {
	return tea.Batch(
		m.spinner.Tick,
		fetchPRsCmd(m.client, m.config.Orgs, m.latestFetchID),
		tickCmd(m.config.PollInterval),
	)
}

func (m model) beginFetch() (model, tea.Cmd) {
	requestID := m.nextFetchID
	m.nextFetchID++
	m.latestFetchID = requestID
	m.loading = true
	return m, fetchPRsCmd(m.client, m.config.Orgs, requestID)
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

	case tickMsg:
		var fetchCmd tea.Cmd
		m, fetchCmd = m.beginFetch()
		return m, tea.Batch(
			fetchCmd,
			tickCmd(m.config.PollInterval),
		)

	case delayedRefreshMsg:
		return m.beginFetch()

	case prsLoadedMsg:
		if msg.requestID != m.latestFetchID {
			return m, nil
		}
		m.loading = false
		if msg.err != nil {
			slog.Error("failed to fetch PRs", "error", msg.err)
			m.err = msg.err
			return m, nil
		}
		m.err = nil
		m.prevPRs = m.prs
		m.prs = msg.prs
		m.lastFetch = time.Now()

		m.checkTransitions()
		deleteCmds := m.detectMerged()
		m = m.cleanupAuto()

		// Clamp cursor to combined list
		items := m.displayItems()
		if m.cursor >= len(items) {
			m.cursor = max(0, len(items)-1)
		}

		cmds := m.autoCmds()
		cmds = append(cmds, deleteCmds...)
		if len(cmds) > 0 {
			return m, tea.Batch(cmds...)
		}

		return m, nil

	case commentPostedMsg:
		if msg.err != nil {
			slog.Error("failed to post merge comment", "pr", msg.pr.RepoShort(), "error", msg.err)
			m.status = errorStyle.Render("✗ Failed to post " + m.config.MergeComment + ": " + msg.err.Error())
		} else if msg.duplicate {
			slog.Info("skipped duplicate merge comment", "pr", msg.pr.RepoShort())
			m.status = indicatorWarn.Render("⚠ " + m.config.MergeComment + " already posted less than a minute ago")
		} else {
			slog.Info("posted merge comment", "pr", msg.pr.RepoShort())
			m.status = successStyle.Render(fmt.Sprintf("✓ Posted %s on %s", m.config.MergeComment, msg.pr.RepoShort()))
		}
		var fetchCmd tea.Cmd
		m, fetchCmd = m.beginFetch()
		return m, tea.Batch(
			clearStatusCmd(),
			fetchCmd,
			delayedRefreshCmd(15*time.Second),
			delayedRefreshCmd(30*time.Second),
		)

	case checksRerunMsg:
		if msg.err != nil {
			slog.Error("failed to re-run checks", "pr", msg.pr.RepoShort(), "error", msg.err)
			m.status = errorStyle.Render("✗ Re-run failed: " + msg.err.Error())
		} else {
			slog.Info("re-running failed checks", "pr", msg.pr.RepoShort())
			m.status = successStyle.Render(fmt.Sprintf("✓ Re-running failed jobs on %s", msg.pr.RepoShort()))
		}
		var fetchCmd tea.Cmd
		m, fetchCmd = m.beginFetch()
		return m, tea.Batch(clearStatusCmd(), fetchCmd)

	case rebaseFinishedMsg:
		if msg.err != nil {
			slog.Error("rebase failed", "error", msg.err)
			m.status = errorStyle.Render("✗ Rebase failed: " + msg.err.Error())
		} else {
			slog.Info("rebase completed successfully")
			m.status = successStyle.Render("✓ Rebase complete!")
		}
		var fetchCmd tea.Cmd
		m, fetchCmd = m.beginFetch()
		return m, tea.Batch(clearStatusCmd(), fetchCmd)

	case autoMergeResultMsg:
		if msg.failed {
			slog.Error("automerge gave up", "pr", msg.prKey)
			m.mergeFailed[msg.prKey] = true
			m.status = errorStyle.Render(fmt.Sprintf("⚡ %s: %s posted %d times, PR still open — giving up", msg.prKey, m.config.MergeComment, m.config.AutoMaxRetries))
			sendNotification("Automerge failed", msg.prKey+" — giving up")
		} else if msg.posted {
			slog.Info("automerge posted merge comment", "pr", msg.prKey)
			m.status = successStyle.Render(fmt.Sprintf("⚡ Automerge: posted %s on %s", m.config.MergeComment, msg.prKey))
			return m, tea.Batch(
				clearStatusCmd(),
				delayedRefreshCmd(15*time.Second),
				delayedRefreshCmd(30*time.Second),
			)
		} else if msg.err != nil {
			slog.Error("automerge error", "pr", msg.prKey, "error", msg.err)
		}
		return m, clearStatusCmd()

	case autoRerunResultMsg:
		if msg.failed {
			slog.Error("autorerun gave up", "pr", msg.prKey)
			m.rerunFailed[msg.prKey] = true
			m.status = errorStyle.Render(fmt.Sprintf("⚡ %s: re-ran %d times, still failing — giving up", msg.prKey, m.config.AutoMaxRetries))
			sendNotification("Autorerun failed", msg.prKey+" — giving up")
		} else if msg.reran {
			slog.Info("autorerun triggered", "pr", msg.prKey)
			if msg.reranAt.IsZero() {
				msg.reranAt = time.Now()
			}
			m.lastAutoRerun[msg.prKey] = msg.reranAt
			if err := m.saveDB(); err != nil {
				slog.Error("failed to save db", "error", err)
				m.status = errorStyle.Render("⚡ Autorerun triggered, but failed to save db.yaml: " + err.Error())
				return m, clearStatusCmd()
			}
			m.status = successStyle.Render(fmt.Sprintf("⚡ Autorerun: re-running failed jobs on %s", msg.prKey))
		} else if msg.unavailable {
			slog.Info("autorerun skipped, no failed GitHub Actions runs", "pr", msg.prKey)
		} else if msg.err != nil {
			slog.Error("autorerun error", "pr", msg.prKey, "error", msg.err)
		}
		return m, clearStatusCmd()

	case mergedCheckMsg:
		if msg.err != nil {
			slog.Error("merged PR check/cleanup error", "pr", msg.prKey, "error", msg.err)
		} else if msg.open {
			delete(m.recentlyMerged, msg.prKey)
			slog.Info("PR is still open, keeping it out of merged/closed list", "pr", msg.prKey)
		} else if msg.deleted {
			if mp := m.recentlyMerged[msg.prKey]; mp != nil {
				mp.Confirmed = true
				mp.Merged = true
			}
			slog.Info("deleted merged branch", "pr", msg.prKey, "branch", msg.branch)
		} else if !msg.merged {
			if mp := m.recentlyMerged[msg.prKey]; mp != nil {
				mp.Confirmed = true
				mp.Merged = false
			}
			slog.Info("PR closed without merge", "pr", msg.prKey)
		} else if mp := m.recentlyMerged[msg.prKey]; mp != nil {
			mp.Confirmed = true
			mp.Merged = true
		}
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
	case "a":
		return m.toggleAuto(m.autoMerge, m.mergeFailed, "automerge")
	case "A":
		return m.toggleAuto(m.autoRerun, m.rerunFailed, "autorerun")
	case "o", "enter":
		return m.openInBrowser()
	case "R", "ctrl+r":
		m.status = ""
		return m.beginFetch()
	}

	return m, nil
}

// Actions — all check for merged PRs and bail if so (except open)

func (m model) doMerge() (tea.Model, tea.Cmd) {
	item, ok := m.selectedItem()
	if !ok || item.merged {
		return m, nil
	}

	if item.pr.OverallStatus != StatusReady {
		m.status = errorStyle.Render("✗ PR not ready to merge — need synced base + passing CI")
		return m, clearStatusCmd()
	}

	m.status = fmt.Sprintf("Posting %s on %s...", m.config.MergeComment, item.pr.RepoShort())
	return m, postCommentCmd(m.client, item.pr, m.config.MergeComment)
}

func (m model) doRebase() (tea.Model, tea.Cmd) {
	item, ok := m.selectedItem()
	if !ok || item.merged {
		return m, nil
	}

	if item.pr.BaseStatus == BaseConflict {
		m.status = errorStyle.Render("✗ PR has conflicts — resolve manually")
		return m, clearStatusCmd()
	}

	script, err := buildRebaseScript(m.config.DataDir, item.pr, m.config.RebaseCountdown)
	if err != nil {
		m.status = errorStyle.Render("✗ " + err.Error())
		return m, clearStatusCmd()
	}

	cmd := exec.Command("bash", "-c", script)
	return m, tea.ExecProcess(cmd, func(err error) tea.Msg {
		return rebaseFinishedMsg{err: err}
	})
}

func (m model) doRerunChecks() (tea.Model, tea.Cmd) {
	item, ok := m.selectedItem()
	if !ok || item.merged {
		return m, nil
	}

	if item.pr.CIStatus != CIFailed {
		m.status = errorStyle.Render("✗ No failed checks to re-run")
		return m, clearStatusCmd()
	}
	if item.pr.FailedWorkflowRunsKnown && item.pr.FailedWorkflowRuns == 0 {
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

func (m model) openInBrowser() (tea.Model, tea.Cmd) {
	item, ok := m.selectedItem()
	if !ok {
		return m, nil
	}
	// Works on both active and merged PRs
	_ = exec.Command("open", item.pr.URL).Start()
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
	return saveDB(m.config.DataDir, m.autoMerge, m.autoRerun, m.lastAutoRerun)
}

// Auto logic

func (m model) isMergeFailed(key string) bool {
	return m.autoMerge[key] && m.mergeFailed[key]
}

func (m model) isRerunFailed(key string) bool {
	return m.autoRerun[key] && m.rerunFailed[key]
}

func (m *model) autoCmds() []tea.Cmd {
	var cmds []tea.Cmd

	for _, pr := range m.prs {
		if cmd := m.autoMergeCmd(pr); cmd != nil {
			cmds = append(cmds, cmd)
		}
		if cmd := m.autoRerunCmd(pr); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	return cmds
}

func (m *model) immediateAutoCmd(pr PR, label string) tea.Cmd {
	switch label {
	case "automerge":
		return m.autoMergeCmd(pr)
	case "autorerun":
		return m.autoRerunCmd(pr)
	default:
		return nil
	}
}

func (m *model) autoMergeCmd(pr PR) tea.Cmd {
	key := pr.RepoShort()
	if !m.shouldAutoMerge(pr) || !m.markAutoCheck("merge", key) {
		return nil
	}

	return autoMergeCmd(m.client, pr, m.config)
}

func (m *model) autoRerunCmd(pr PR) tea.Cmd {
	key := pr.RepoShort()
	if !m.shouldAutoRerun(pr) || !m.markAutoCheck("rerun", key) {
		return nil
	}

	return autoRerunCmd(m.client, pr, m.config.AutoMaxRetries)
}

func (m model) shouldAutoMerge(pr PR) bool {
	key := pr.RepoShort()
	return m.autoMerge[key] && !m.mergeFailed[key] && pr.OverallStatus == StatusReady
}

func (m model) shouldAutoRerun(pr PR) bool {
	key := pr.RepoShort()
	if !m.autoRerun[key] || m.rerunFailed[key] || pr.CIStatus != CIFailed ||
		!pr.FailedWorkflowRunsKnown || pr.FailedWorkflowRuns <= 0 {
		return false
	}

	last := m.lastAutoRerun[key]
	return last.IsZero() || time.Since(last) >= m.config.AutoCooldown
}

func (m *model) markAutoCheck(kind, key string) bool {
	if m.lastAutoCheck == nil {
		m.lastAutoCheck = make(map[string]time.Time)
	}

	checkKey := kind + ":" + key
	if time.Since(m.lastAutoCheck[checkKey]) < m.config.AutoCooldown {
		return false
	}

	m.lastAutoCheck[checkKey] = time.Now()
	return true
}

func (m model) clearAutoCheck(label, key string) {
	switch label {
	case "automerge":
		delete(m.lastAutoCheck, "merge:"+key)
	case "autorerun":
		delete(m.lastAutoCheck, "rerun:"+key)
	}
}

// Merged PR tracking

func (m model) detectMerged() []tea.Cmd {
	if len(m.prevPRs) == 0 {
		return nil
	}

	currentKeys := make(map[string]bool, len(m.prs))
	for _, pr := range m.prs {
		currentKeys[pr.RepoShort()] = true
	}

	var cmds []tea.Cmd
	for _, pr := range m.prevPRs {
		key := pr.RepoShort()
		if !currentKeys[key] && m.recentlyMerged[key] == nil {
			slog.Info("PR disappeared (merged/closed)", "pr", key)
			m.recentlyMerged[key] = &mergedPR{PR: pr, GoneAt: time.Now()}

			deleteBranch := m.config.DeleteMergedBranches &&
				pr.HeadRepo != "" && pr.HeadBranch != "" &&
				pr.HeadRepo != pr.Repo && // only delete on forks, never same-repo branches
				strings.EqualFold(pr.HeadRepoOwner, m.client.getUsername())
			cmds = append(cmds, checkMergedPRCmd(m.client, pr, deleteBranch))
		}
	}

	for key, mp := range m.recentlyMerged {
		if time.Since(mp.GoneAt) >= m.config.MergedDisplayDuration {
			delete(m.recentlyMerged, key)
		}
	}

	return cmds
}

func (m model) cleanupAuto() model {
	openPRs := make(map[string]bool, len(m.prs))
	for _, pr := range m.prs {
		openPRs[pr.RepoShort()] = true
	}

	changed := false
	for key := range m.autoMerge {
		if !openPRs[key] && m.isConfirmedGone(key) {
			delete(m.autoMerge, key)
			changed = true
		}
	}
	for key := range m.autoRerun {
		if !openPRs[key] && m.isConfirmedGone(key) {
			delete(m.autoRerun, key)
			delete(m.lastAutoRerun, key)
			changed = true
		}
	}

	if changed {
		if err := m.saveDB(); err != nil {
			slog.Error("failed to save db", "error", err)
			m.status = errorStyle.Render("✗ Failed to save db.yaml: " + err.Error())
		}
	}

	return m
}

func (m model) isConfirmedGone(key string) bool {
	mp := m.recentlyMerged[key]
	return mp != nil && mp.Confirmed
}

func (m model) checkTransitions() {
	if len(m.prevPRs) == 0 {
		return
	}

	prevMap := make(map[string]PR)
	for _, pr := range m.prevPRs {
		prevMap[pr.RepoShort()] = pr
	}

	for _, pr := range m.prs {
		prev, exists := prevMap[pr.RepoShort()]
		if !exists {
			continue
		}

		if prev.OverallStatus != StatusReady && pr.OverallStatus == StatusReady {
			slog.Info("PR became ready to merge", "pr", pr.RepoShort())
			sendNotification("Ready to merge", fmt.Sprintf("%s — %s", pr.RepoShort(), pr.Title))
		}

		if prev.CIStatus != CIFailed && pr.CIStatus == CIFailed {
			slog.Info("checks failed", "pr", pr.RepoShort())
			sendNotification("Checks failed", fmt.Sprintf("%s — %s", pr.RepoShort(), pr.Title))
		}

		if prev.BaseStatus == BaseSynced && pr.BaseStatus == BaseBehind {
			slog.Info("PR became stale", "pr", pr.RepoShort())
			sendNotification("PR needs rebase", fmt.Sprintf("%s — %s", pr.RepoShort(), pr.Title))
		}
	}
}

// View

func (m model) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder

	b.WriteString(m.renderHeader())
	b.WriteString("\n\n")

	items := m.displayItems()

	if m.loading && len(items) == 0 {
		b.WriteString("  " + m.spinner.View() + " Loading PRs...")
		b.WriteString("\n")
	} else if m.err != nil && len(items) == 0 {
		b.WriteString(errorStyle.Render("  Error: " + m.err.Error()))
		b.WriteString("\n")
	} else if len(items) == 0 {
		b.WriteString("  No open PRs found.\n")
	} else {
		b.WriteString(m.renderItemList(items))
	}

	if m.status != "" {
		b.WriteString("\n")
		b.WriteString("  " + m.status)
		b.WriteString("\n")
	}

	if m.err != nil && len(m.prs) > 0 {
		b.WriteString("\n")
		b.WriteString(errorStyle.Render("  Fetch error: " + m.err.Error()))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(m.renderFooter())

	return b.String()
}

func (m model) renderHeader() string {
	title := headerStyle.Render("sidero-rebaser")

	var info string
	if m.loading {
		info = m.spinner.View() + " refreshing"
	} else if !m.lastFetch.IsZero() {
		ago := time.Since(m.lastFetch).Truncate(time.Second)
		info = fmt.Sprintf("↻ %s ago", ago)
	}

	prCount := ""
	if len(m.prs) > 0 {
		prCount = fmt.Sprintf(" · %d PRs", len(m.prs))
	}

	right := headerInfoStyle.Render(info + prCount)
	gap := max(m.width-lipgloss.Width(title)-lipgloss.Width(right)-2, 1)

	return title + strings.Repeat(" ", gap) + right
}

func (m model) renderItemList(items []displayItem) string {
	var b strings.Builder

	linesPerPR := 4
	availableHeight := max(m.height-8, linesPerPR)
	maxVisible := max(availableHeight/linesPerPR, 1)

	scrollOffset := 0
	if m.cursor >= maxVisible {
		scrollOffset = m.cursor - maxVisible + 1
	}

	end := min(scrollOffset+maxVisible, len(items))

	for i := scrollOffset; i < end; i++ {
		selected := i == m.cursor
		b.WriteString(m.renderItem(items[i], selected))
		if i < end-1 {
			b.WriteString("\n")
		}
	}

	if len(items) > maxVisible {
		fmt.Fprintf(&b, "\n  %s\n",
			prDescStyle.Render(fmt.Sprintf("(%d/%d shown — scroll with j/k)", min(maxVisible, len(items)), len(items))))
	}

	return b.String()
}

func (m model) renderItem(item displayItem, selected bool) string {
	if item.merged {
		return m.renderMergedItem(item, selected)
	}
	return m.renderActivePR(item.pr, selected)
}

func (m model) renderActivePR(pr PR, selected bool) string {
	maxTitleWidth := max(m.width-30, 20)
	key := pr.RepoShort()

	repoRef := prRepoStyle.Render(key)
	badge := m.badgeFor(key, pr.OverallStatus)
	gap := max(m.width-lipgloss.Width(repoRef)-lipgloss.Width(badge)-6, 1)
	line1 := repoRef + strings.Repeat(" ", gap) + badge

	title := truncateDisplayWidth(pr.Title, maxTitleWidth)
	line2 := prTitleStyle.Render(title)

	base := "Base " + baseIndicator(pr.BaseStatus)
	ci := "CI " + ciIndicator(pr.CIStatus, pr.Checks)
	review := "Review " + reviewIndicator(pr.ReviewStatus)
	auto := m.autoIndicators(key)
	line3 := prDescStyle.Render(base + "  " + ci + "  " + review + auto)

	content := line1 + "\n" + line2 + "\n" + line3 + "\n"

	if selected {
		return selectedStyle.Render(content)
	}
	return normalStyle.Render(content)
}

func (m model) renderMergedItem(item displayItem, selected bool) string {
	maxTitleWidth := max(m.width-30, 20)

	repoRef := prRepoStyle.Render(item.pr.RepoShort())
	confirmed := false
	merged := false
	if mp := m.recentlyMerged[item.pr.RepoShort()]; mp != nil {
		confirmed = mp.Confirmed
		merged = mp.Merged
	}
	badge := badgePending.Render("CHECKING")
	if confirmed && merged {
		badge = badgeMerged.Render("MERGED")
	} else if confirmed {
		badge = badgeDraft.Render("CLOSED")
	}
	gap := max(m.width-lipgloss.Width(repoRef)-lipgloss.Width(badge)-6, 1)
	line1 := repoRef + strings.Repeat(" ", gap) + badge

	title := truncateDisplayWidth(item.pr.Title, maxTitleWidth)
	line2 := prTitleStyle.Render(title)

	ago := time.Since(item.goneAt).Truncate(time.Second)
	desc := fmt.Sprintf("Checking merge state (%s ago)", ago)
	if confirmed && merged {
		desc = fmt.Sprintf("Merged %s ago", ago)
	} else if confirmed {
		desc = fmt.Sprintf("Closed %s ago", ago)
	}
	line3 := prDescStyle.Render(desc)

	content := line1 + "\n" + line2 + "\n" + line3 + "\n"

	if selected {
		return selectedStyle.Render(content)
	}
	return normalStyle.Render(content)
}

func (m model) badgeFor(key string, status OverallStatus) string {
	if m.isMergeFailed(key) {
		return badgeMergeFailed.Render("MERGE FAIL")
	}
	if m.isRerunFailed(key) {
		return badgeRerunFailed.Render("RERUN FAIL")
	}
	return statusBadge(status)
}

func (m model) autoIndicators(key string) string {
	var parts []string

	if m.isMergeFailed(key) {
		parts = append(parts, badgeMergeFailed.Render("⚡ merge gave up"))
	} else if m.autoMerge[key] {
		parts = append(parts, indicatorAuto.Render("⚡ automerge"))
	}

	if m.isRerunFailed(key) {
		parts = append(parts, badgeRerunFailed.Render("⚡ rerun gave up"))
	} else if m.autoRerun[key] {
		parts = append(parts, indicatorAuto.Render("⚡ autorerun"))
	}

	if len(parts) == 0 {
		return ""
	}
	return "  " + strings.Join(parts, " ")
}

func truncateDisplayWidth(s string, maxWidth int) string {
	if maxWidth <= 0 || s == "" {
		return ""
	}
	if lipgloss.Width(s) <= maxWidth {
		return s
	}

	const ellipsis = "…"
	ellipsisWidth := lipgloss.Width(ellipsis)
	if maxWidth <= ellipsisWidth {
		return ellipsis
	}

	limit := maxWidth - ellipsisWidth
	var b strings.Builder
	width := 0
	for _, r := range s {
		part := string(r)
		partWidth := lipgloss.Width(part)
		if width+partWidth > limit {
			break
		}
		b.WriteRune(r)
		width += partWidth
	}

	return b.String() + ellipsis
}

func (m model) renderFooter() string {
	keys := []struct{ key, desc string }{
		{"m", "merge"},
		{"r", "rebase"},
		{"t", "re-run"},
		{"a", "automerge"},
		{"A", "autorerun"},
		{"o", "open"},
		{"R", "refresh"},
		{"q", "quit"},
	}

	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = footerKeyStyle.Render(k.key) + " " + k.desc
	}

	return footerStyle.Render(strings.Join(parts, "  ·  "))
}

// Commands

func tickCmd(interval time.Duration) tea.Cmd {
	return tea.Tick(interval, func(_ time.Time) tea.Msg {
		return tickMsg{}
	})
}

func delayedRefreshCmd(delay time.Duration) tea.Cmd {
	return tea.Tick(delay, func(_ time.Time) tea.Msg {
		return delayedRefreshMsg{}
	})
}

func fetchPRsCmd(client *GitHubClient, orgs []string, requestID int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		prs, err := client.fetchPRs(ctx, orgs)
		return prsLoadedMsg{requestID: requestID, prs: prs, err: err}
	}
}

func postCommentCmd(client *GitHubClient, pr PR, comment string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		dup, err := client.postComment(ctx, pr.Repo, pr.Number, comment)
		return commentPostedMsg{pr: pr, err: err, duplicate: dup}
	}
}

func rerunChecksCmd(client *GitHubClient, pr PR) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		err := client.rerunFailedJobs(ctx, pr.Repo, pr.HeadSHA)
		return checksRerunMsg{pr: pr, err: err}
	}
}

func clearStatusCmd() tea.Cmd {
	return tea.Tick(5*time.Second, func(_ time.Time) tea.Msg {
		return clearStatusMsg{}
	})
}

// checkMergedPRCmd verifies whether a PR was merged, and optionally deletes the head branch.
func checkMergedPRCmd(client *GitHubClient, pr PR, deleteBranch bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		key := pr.RepoShort()

		state, err := client.getPRState(ctx, pr.Repo, pr.Number)
		if err != nil {
			return mergedCheckMsg{prKey: key, branch: pr.HeadBranch, err: fmt.Errorf("check merged: %w", err)}
		}
		if strings.EqualFold(state.State, "open") {
			return mergedCheckMsg{prKey: key, branch: pr.HeadBranch, open: true}
		}

		if !state.Merged || !deleteBranch {
			return mergedCheckMsg{prKey: key, branch: pr.HeadBranch, merged: state.Merged}
		}

		err = client.deleteBranch(ctx, pr.HeadRepo, pr.HeadBranch)
		if err != nil {
			return mergedCheckMsg{prKey: key, branch: pr.HeadBranch, merged: true, err: err}
		}

		return mergedCheckMsg{prKey: key, branch: pr.HeadBranch, merged: true, deleted: true}
	}
}
