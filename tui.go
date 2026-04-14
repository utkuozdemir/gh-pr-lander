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
	prs []PR
	err error
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

type branchDeletedMsg struct {
	prKey  string
	branch string
	err    error
}

// Model

type mergedPR struct {
	PR     PR
	GoneAt time.Time
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
	mergeFailed    map[string]bool      // discovered from API, in-memory
	rerunFailed    map[string]bool      // discovered from API, in-memory
	lastAutoCheck  map[string]time.Time // cooldown gate to avoid API spam
	recentlyMerged map[string]*mergedPR // PRs that disappeared (merged/closed)
	quitting       bool
}

func newModel(cfg Config, client *GitHubClient) model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(colorCyan)

	am, ar := loadDB(cfg.DataDir)

	return model{
		config:         cfg,
		client:         client,
		loading:        true,
		spinner:        s,
		lastRerun:      make(map[string]time.Time),
		autoMerge:      am,
		autoRerun:      ar,
		mergeFailed:    make(map[string]bool),
		rerunFailed:    make(map[string]bool),
		lastAutoCheck:  make(map[string]time.Time),
		recentlyMerged: make(map[string]*mergedPR),
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
		fetchPRsCmd(m.client, m.config.Orgs),
		tickCmd(m.config.PollInterval),
	)
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
		return m, tea.Batch(
			fetchPRsCmd(m.client, m.config.Orgs),
			tickCmd(m.config.PollInterval),
		)

	case delayedRefreshMsg:
		return m, fetchPRsCmd(m.client, m.config.Orgs)

	case prsLoadedMsg:
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
		m.cleanupAuto()

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
		return m, tea.Batch(
			clearStatusCmd(),
			fetchPRsCmd(m.client, m.config.Orgs),
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
		return m, tea.Batch(clearStatusCmd(), fetchPRsCmd(m.client, m.config.Orgs))

	case rebaseFinishedMsg:
		if msg.err != nil {
			slog.Error("rebase failed", "error", msg.err)
			m.status = errorStyle.Render("✗ Rebase failed: " + msg.err.Error())
		} else {
			slog.Info("rebase completed successfully")
			m.status = successStyle.Render("✓ Rebase complete!")
		}
		return m, tea.Batch(clearStatusCmd(), fetchPRsCmd(m.client, m.config.Orgs))

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
			m.status = successStyle.Render(fmt.Sprintf("⚡ Autorerun: re-running failed jobs on %s", msg.prKey))
		} else if msg.err != nil {
			slog.Error("autorerun error", "pr", msg.prKey, "error", msg.err)
		}
		return m, clearStatusCmd()

	case branchDeletedMsg:
		if msg.err != nil {
			slog.Error("failed to delete branch", "pr", msg.prKey, "branch", msg.branch, "error", msg.err)
		} else {
			slog.Info("deleted merged branch", "pr", msg.prKey, "branch", msg.branch)
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
		m.loading = true
		m.status = ""
		return m, fetchPRsCmd(m.client, m.config.Orgs)
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

	if entries[key] {
		delete(entries, key)
		delete(failedMap, key)
		delete(m.lastAutoCheck, key)
		slog.Info(label+" disabled", "pr", key)
		m.status = indicatorAuto.Render("⚡ " + label + " disabled for " + key)
	} else {
		entries[key] = true
		slog.Info(label+" enabled", "pr", key)
		m.status = indicatorAuto.Render("⚡ " + label + " enabled for " + key)
	}

	saveDB(m.config.DataDir, m.autoMerge, m.autoRerun)
	return m, clearStatusCmd()
}

// Auto logic

func (m model) isMergeFailed(key string) bool {
	return m.autoMerge[key] && m.mergeFailed[key]
}

func (m model) isRerunFailed(key string) bool {
	return m.autoRerun[key] && m.rerunFailed[key]
}

func (m model) autoCmds() []tea.Cmd {
	var cmds []tea.Cmd

	for _, pr := range m.prs {
		key := pr.RepoShort()

		if m.autoMerge[key] && !m.mergeFailed[key] && pr.OverallStatus == StatusReady {
			checkKey := "merge:" + key
			if time.Since(m.lastAutoCheck[checkKey]) >= m.config.AutoCooldown {
				m.lastAutoCheck[checkKey] = time.Now()
				cmds = append(cmds, autoMergeCmd(m.client, pr, m.config))
			}
		}

		if m.autoRerun[key] && !m.rerunFailed[key] && pr.CIStatus == CIFailed {
			checkKey := "rerun:" + key
			if time.Since(m.lastAutoCheck[checkKey]) >= m.config.AutoCooldown {
				m.lastAutoCheck[checkKey] = time.Now()
				cmds = append(cmds, autoRerunCmd(m.client, pr, m.config.AutoMaxRetries))
			}
		}
	}

	return cmds
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

			if m.config.DeleteMergedBranches && pr.HeadRepo != "" && pr.HeadBranch != "" {
				cmds = append(cmds, deleteMergedBranchCmd(m.client, pr))
			}
		}
	}

	for key, mp := range m.recentlyMerged {
		if time.Since(mp.GoneAt) >= m.config.MergedDisplayDuration {
			delete(m.recentlyMerged, key)
		}
	}

	return cmds
}

func (m model) cleanupAuto() {
	openPRs := make(map[string]bool, len(m.prs))
	for _, pr := range m.prs {
		openPRs[pr.RepoShort()] = true
	}

	changed := false
	for key := range m.autoMerge {
		if !openPRs[key] {
			delete(m.autoMerge, key)
			changed = true
		}
	}
	for key := range m.autoRerun {
		if !openPRs[key] {
			delete(m.autoRerun, key)
			changed = true
		}
	}

	if changed {
		saveDB(m.config.DataDir, m.autoMerge, m.autoRerun)
	}
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

	title := pr.Title
	if len(title) > maxTitleWidth {
		title = title[:maxTitleWidth-1] + "…"
	}
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
	badge := badgeMerged.Render("MERGED")
	gap := max(m.width-lipgloss.Width(repoRef)-lipgloss.Width(badge)-6, 1)
	line1 := repoRef + strings.Repeat(" ", gap) + badge

	title := item.pr.Title
	if len(title) > maxTitleWidth {
		title = title[:maxTitleWidth-1] + "…"
	}
	line2 := prTitleStyle.Render(title)

	ago := time.Since(item.goneAt).Truncate(time.Second)
	line3 := prDescStyle.Render(fmt.Sprintf("Merged %s ago", ago))

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

func fetchPRsCmd(client *GitHubClient, orgs []string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		prs, err := client.fetchPRs(ctx, orgs)
		return prsLoadedMsg{prs: prs, err: err}
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

// deleteMergedBranchCmd verifies a PR is truly merged, then deletes its head branch.
func deleteMergedBranchCmd(client *GitHubClient, pr PR) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		key := pr.RepoShort()

		merged, err := client.isPRMerged(ctx, pr.Repo, pr.Number)
		if err != nil {
			return branchDeletedMsg{prKey: key, branch: pr.HeadBranch, err: fmt.Errorf("check merged: %w", err)}
		}

		if !merged {
			slog.Info("PR closed but not merged, skipping branch delete", "pr", key)
			return branchDeletedMsg{prKey: key, branch: pr.HeadBranch, err: nil}
		}

		err = client.deleteBranch(ctx, pr.HeadRepo, pr.HeadBranch)
		return branchDeletedMsg{prKey: key, branch: pr.HeadBranch, err: err}
	}
}
