package main

import (
	"context"
	"fmt"
	"log/slog"
	"os/exec"
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

// Model

type model struct {
	config      Config
	client      *GitHubClient
	prs         []PR
	prevPRs     []PR // previous state for transition detection
	cursor      int
	width       int
	height      int
	loading     bool
	err         error
	lastFetch   time.Time
	status      string // temporary status message
	spinner     spinner.Model
	lastRerun   map[string]time.Time // PR key -> last re-run time
	autoMerge   map[string]bool      // PR key -> auto-merge enabled
	mergeFailed map[string]bool      // PR key -> 3+ /m with no merge
	lastAutoM   map[string]time.Time // PR key -> last auto /m attempt
	quitting    bool
}

func newModel(cfg Config, client *GitHubClient) model {
	s := spinner.New()
	s.Spinner = spinner.Dot
	s.Style = lipgloss.NewStyle().Foreground(colorCyan)

	return model{
		config:      cfg,
		client:      client,
		loading:     true,
		spinner:     s,
		lastRerun:   make(map[string]time.Time),
		autoMerge:   loadAutoMerge(cfg.DataDir),
		mergeFailed: make(map[string]bool),
		lastAutoM:   make(map[string]time.Time),
	}
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

		// Clamp cursor
		if m.cursor >= len(m.prs) {
			m.cursor = max(0, len(m.prs)-1)
		}

		// Check for state transitions and notify
		m.checkTransitions()

		// Clean up auto entries for PRs no longer open
		m.cleanupAutoMerge()

		// Trigger auto-merge for ready PRs
		if cmds := m.autoMergeCmds(); len(cmds) > 0 {
			return m, tea.Batch(cmds...)
		}

		return m, nil

	case commentPostedMsg:
		if msg.err != nil {
			slog.Error("failed to post /m", "pr", msg.pr.RepoShort(), "error", msg.err)
			m.status = errorStyle.Render("✗ Failed to post /m: " + msg.err.Error())
		} else if msg.duplicate {
			slog.Info("skipped duplicate /m", "pr", msg.pr.RepoShort())
			m.status = indicatorWarn.Render("⚠ /m already posted less than a minute ago")
		} else {
			slog.Info("posted /m", "pr", msg.pr.RepoShort())
			m.status = successStyle.Render(fmt.Sprintf("✓ Posted /m on %s", msg.pr.RepoShort()))
		}
		return m, tea.Batch(
			clearStatusCmd(),
			fetchPRsCmd(m.client, m.config.Orgs),
		)

	case checksRerunMsg:
		if msg.err != nil {
			slog.Error("failed to re-run checks", "pr", msg.pr.RepoShort(), "error", msg.err)
			m.status = errorStyle.Render("✗ Re-run failed: " + msg.err.Error())
		} else {
			slog.Info("re-running failed checks", "pr", msg.pr.RepoShort())
			m.status = successStyle.Render(fmt.Sprintf("✓ Re-running failed jobs on %s", msg.pr.RepoShort()))
		}
		return m, tea.Batch(
			clearStatusCmd(),
			fetchPRsCmd(m.client, m.config.Orgs),
		)

	case rebaseFinishedMsg:
		if msg.err != nil {
			slog.Error("rebase failed", "error", msg.err)
			m.status = errorStyle.Render("✗ Rebase failed: " + msg.err.Error())
		} else {
			slog.Info("rebase completed successfully")
			m.status = successStyle.Render("✓ Rebase complete!")
		}
		return m, tea.Batch(
			clearStatusCmd(),
			fetchPRsCmd(m.client, m.config.Orgs),
		)

	case autoMergeResultMsg:
		if msg.mergeFailed {
			slog.Error("auto-merge gave up", "pr", msg.prKey, "reason", "3+ /m comments with no merge")
			m.mergeFailed[msg.prKey] = true
			m.status = errorStyle.Render(fmt.Sprintf("⚡ %s: /m posted 3 times but PR still open — giving up", msg.prKey))
			sendNotification("Auto-merge failed", msg.prKey+" — /m posted 3 times, PR still open")
		} else if msg.posted {
			slog.Info("auto-merge posted /m", "pr", msg.prKey)
			m.lastAutoM[msg.prKey] = time.Now()
			m.status = successStyle.Render(fmt.Sprintf("⚡ Auto-posted /m on %s", msg.prKey))
		} else if msg.err != nil {
			slog.Error("auto-merge error", "pr", msg.prKey, "error", msg.err)
		}
		// tooSoon: silently skip, will retry next cycle
		return m, clearStatusCmd()

	case clearStatusMsg:
		m.status = ""
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}

	return m, nil
}

func (m model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "q", "ctrl+c":
		m.quitting = true
		return m, tea.Quit

	case "j", "down":
		if m.cursor < len(m.prs)-1 {
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
		m.cursor = max(0, len(m.prs)-1)
		return m, nil

	case "m":
		return m.doMerge()

	case "r":
		return m.doRebase()

	case "t":
		return m.doRerunChecks()

	case "a":
		return m.toggleAutoMerge()

	case "o", "enter":
		return m.openInBrowser()

	case "R", "ctrl+r":
		m.loading = true
		m.status = ""
		return m, fetchPRsCmd(m.client, m.config.Orgs)
	}

	return m, nil
}

func (m model) doMerge() (tea.Model, tea.Cmd) {
	if len(m.prs) == 0 {
		return m, nil
	}
	pr := m.prs[m.cursor]

	if pr.OverallStatus != StatusReady {
		m.status = errorStyle.Render("✗ PR not ready to merge — need synced base + passing CI")
		return m, clearStatusCmd()
	}

	m.status = fmt.Sprintf("Posting %s on %s...", m.config.MergeComment, pr.RepoShort())
	return m, postCommentCmd(m.client, pr, m.config.MergeComment)
}

func (m model) doRebase() (tea.Model, tea.Cmd) {
	if len(m.prs) == 0 {
		return m, nil
	}
	pr := m.prs[m.cursor]

	if pr.BaseStatus == BaseConflict {
		m.status = errorStyle.Render("✗ PR has conflicts — resolve manually")
		return m, clearStatusCmd()
	}

	script, err := buildRebaseScript(m.config.DataDir, pr, m.config.RebaseCountdown)
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
	if len(m.prs) == 0 {
		return m, nil
	}
	pr := m.prs[m.cursor]

	if pr.CIStatus != CIFailed {
		m.status = errorStyle.Render("✗ No failed checks to re-run")
		return m, clearStatusCmd()
	}

	if last, ok := m.lastRerun[pr.RepoShort()]; ok && time.Since(last) < time.Minute {
		m.status = indicatorWarn.Render("⚠ Already re-ran checks less than a minute ago")
		return m, clearStatusCmd()
	}

	m.lastRerun[pr.RepoShort()] = time.Now()
	m.status = fmt.Sprintf("Re-running failed jobs on %s...", pr.RepoShort())
	return m, rerunChecksCmd(m.client, pr)
}

func (m model) openInBrowser() (tea.Model, tea.Cmd) {
	if len(m.prs) == 0 {
		return m, nil
	}
	pr := m.prs[m.cursor]
	_ = exec.Command("open", pr.URL).Start()
	return m, nil
}

func (m model) toggleAutoMerge() (tea.Model, tea.Cmd) {
	if len(m.prs) == 0 {
		return m, nil
	}
	pr := m.prs[m.cursor]
	key := pr.RepoShort()

	if m.autoMerge[key] {
		delete(m.autoMerge, key)
		delete(m.mergeFailed, key)
		delete(m.lastAutoM, key)
		slog.Info("auto-merge disabled", "pr", key)
		m.status = indicatorAuto.Render("⚡ Auto-merge disabled for " + key)
	} else {
		m.autoMerge[key] = true
		slog.Info("auto-merge enabled", "pr", key)
		m.status = indicatorAuto.Render("⚡ Auto-merge enabled for " + key)
	}

	_ = saveAutoMerge(m.config.DataDir, m.autoMerge)
	return m, clearStatusCmd()
}

// autoMergeCmds returns commands for any auto-merge PRs that are ready.
func (m model) autoMergeCmds() []tea.Cmd {
	var cmds []tea.Cmd

	for _, pr := range m.prs {
		key := pr.RepoShort()
		if !m.autoMerge[key] || m.mergeFailed[key] {
			continue
		}
		if pr.OverallStatus != StatusReady {
			continue
		}
		// Local cooldown — avoid API calls during the cooldown window
		if last, ok := m.lastAutoM[key]; ok && time.Since(last) < m.config.AutoMergeCooldown {
			continue
		}
		cmds = append(cmds, autoMergeCmd(m.client, pr, m.config))
	}

	return cmds
}

// cleanupAutoMerge removes auto entries for PRs no longer in the open list.
func (m model) cleanupAutoMerge() {
	openPRs := make(map[string]bool, len(m.prs))
	for _, pr := range m.prs {
		openPRs[pr.RepoShort()] = true
	}

	changed := false
	for key := range m.autoMerge {
		if !openPRs[key] {
			delete(m.autoMerge, key)
			delete(m.mergeFailed, key)
			delete(m.lastAutoM, key)
			changed = true
		}
	}

	if changed {
		_ = saveAutoMerge(m.config.DataDir, m.autoMerge)
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

		// Notify: became ready to merge
		if prev.OverallStatus != StatusReady && pr.OverallStatus == StatusReady {
			slog.Info("PR became ready to merge", "pr", pr.RepoShort())
			sendNotification(
				"Ready to merge",
				fmt.Sprintf("%s — %s", pr.RepoShort(), pr.Title),
			)
		}

		// Notify: checks failed
		if prev.CIStatus != CIFailed && pr.CIStatus == CIFailed {
			slog.Info("checks failed", "pr", pr.RepoShort())
			sendNotification(
				"Checks failed",
				fmt.Sprintf("%s — %s", pr.RepoShort(), pr.Title),
			)
		}

		// Notify: became stale (someone merged ahead of us)
		if prev.BaseStatus == BaseSynced && pr.BaseStatus == BaseBehind {
			slog.Info("PR became stale", "pr", pr.RepoShort())
			sendNotification(
				"PR needs rebase",
				fmt.Sprintf("%s — %s", pr.RepoShort(), pr.Title),
			)
		}
	}
}

// View

func (m model) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder

	// Header
	b.WriteString(m.renderHeader())
	b.WriteString("\n\n")

	// PRs or loading/error/empty state
	if m.loading && len(m.prs) == 0 {
		b.WriteString("  " + m.spinner.View() + " Loading PRs...")
		b.WriteString("\n")
	} else if m.err != nil && len(m.prs) == 0 {
		b.WriteString(errorStyle.Render("  Error: " + m.err.Error()))
		b.WriteString("\n")
	} else if len(m.prs) == 0 {
		b.WriteString("  No open PRs found.\n")
	} else {
		b.WriteString(m.renderPRList())
	}

	// Status message
	if m.status != "" {
		b.WriteString("\n")
		b.WriteString("  " + m.status)
		b.WriteString("\n")
	}

	// Error from last fetch (shown even when we have PRs)
	if m.err != nil && len(m.prs) > 0 {
		b.WriteString("\n")
		b.WriteString(errorStyle.Render("  Fetch error: " + m.err.Error()))
		b.WriteString("\n")
	}

	// Footer
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

func (m model) renderPRList() string {
	var b strings.Builder

	linesPerPR := 4 // repo line + title line + status line + blank line
	availableHeight := max(
		// header + footer + margins
		m.height-8, linesPerPR)

	maxVisible := max(availableHeight/linesPerPR, 1)

	// Compute scroll offset
	scrollOffset := 0
	if m.cursor >= maxVisible {
		scrollOffset = m.cursor - maxVisible + 1
	}

	end := min(scrollOffset+maxVisible, len(m.prs))

	for i := scrollOffset; i < end; i++ {
		pr := m.prs[i]
		selected := i == m.cursor

		b.WriteString(m.renderPR(pr, selected))

		if i < end-1 {
			b.WriteString("\n")
		}
	}

	if len(m.prs) > maxVisible {
		fmt.Fprintf(&b, "\n  %s",
			prDescStyle.Render(fmt.Sprintf("(%d/%d shown — scroll with j/k)", min(maxVisible, len(m.prs)), len(m.prs))))
		b.WriteString("\n")
	}

	return b.String()
}

func (m model) renderPR(pr PR, selected bool) string {
	maxTitleWidth := max(m.width-30, 20)

	key := pr.RepoShort()

	// Line 1: repo#number + badge
	repoRef := prRepoStyle.Render(key)
	badge := statusBadge(pr.OverallStatus)
	if m.mergeFailed[key] {
		badge = badgeMergeFailed.Render("MERGE FAIL")
	}
	gap := max(m.width-lipgloss.Width(repoRef)-lipgloss.Width(badge)-6, 1)
	line1 := repoRef + strings.Repeat(" ", gap) + badge

	// Line 2: title
	title := pr.Title
	if len(title) > maxTitleWidth {
		title = title[:maxTitleWidth-1] + "…"
	}
	line2 := prTitleStyle.Render(title)

	// Line 3: status indicators
	base := "Base " + baseIndicator(pr.BaseStatus)
	ci := "CI " + ciIndicator(pr.CIStatus, pr.Checks)
	review := "Review " + reviewIndicator(pr.ReviewStatus)
	auto := ""
	if m.mergeFailed[key] {
		auto = "  " + badgeMergeFailed.Render("⚡ gave up")
	} else if m.autoMerge[key] {
		auto = "  " + indicatorAuto.Render("⚡ auto")
	}
	line3 := prDescStyle.Render(base + "  " + ci + "  " + review + auto)

	content := line1 + "\n" + line2 + "\n" + line3 + "\n"

	if selected {
		return selectedStyle.Render(content)
	}
	return normalStyle.Render(content)
}

func (m model) renderFooter() string {
	keys := []struct{ key, desc string }{
		{"m", "merge"},
		{"r", "rebase"},
		{"t", "re-run"},
		{"a", "auto"},
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
