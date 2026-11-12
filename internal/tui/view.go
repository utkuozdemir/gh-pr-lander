package tui

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/utkuozdemir/gh-pr-lander/internal/pr"
)

func (m model) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder

	footer := m.renderFooter()

	b.WriteString(m.renderHeader())
	b.WriteString("\n\n")

	items := m.displayItems()

	switch {
	case m.help:
		b.WriteString(m.renderHelp())
	case m.loading && len(items) == 0:
		b.WriteString("  " + m.spinner.View() + " Loading PRs...")
		b.WriteString("\n")
	case m.err != nil && len(items) == 0:
		b.WriteString(errorStyle.Render("  Error: " + m.err.Error()))
		b.WriteString("\n")
	case len(items) == 0:
		b.WriteString("  No open PRs found.\n")
	default:
		b.WriteString(m.renderItemList(items, strings.Count(footer, "\n")+1))
	}

	// The help view has the screen to itself. A status line under it would
	// push its first lines off the top.
	if m.status != "" && !m.help {
		b.WriteString("\n")
		b.WriteString("  " + m.status)
		b.WriteString("\n")
	}

	if m.err != nil && len(m.PRs) > 0 && !m.help {
		b.WriteString("\n")
		b.WriteString(errorStyle.Render("  Fetch error: " + m.err.Error()))
		b.WriteString("\n")
	}

	b.WriteString("\n")
	b.WriteString(footer)

	return b.String()
}

func (m model) renderHeader() string {
	title := headerStyle.Render("gh-pr-lander")

	var info string
	if m.loading {
		info = m.spinner.View() + " refreshing"
	} else if !m.lastFetch.IsZero() {
		ago := time.Since(m.lastFetch).Truncate(time.Second)
		info = fmt.Sprintf("↻ %s ago", ago)
	}

	prCount := ""
	if len(m.PRs) > 0 {
		prCount = fmt.Sprintf(" · %d PRs", len(m.PRs))
		if len(m.PRs) == 1 {
			prCount = " · 1 PR"
		}
	}

	right := headerInfoStyle.Render(info + prCount)

	// The version is the first thing to give way on a narrow terminal. A
	// header wider than the screen wraps and pushes the list down.
	if m.Config.Version != "" {
		withVersion := title + " " + headerInfoStyle.Render(m.Config.Version)
		if m.width <= 0 || lipgloss.Width(withVersion)+1+lipgloss.Width(right) <= m.width-2 {
			title = withVersion
		}
	}

	gap := max(m.width-lipgloss.Width(title)-lipgloss.Width(right)-2, 1)

	return title + strings.Repeat(" ", gap) + right
}

func (m model) renderItemList(items []displayItem, footerLines int) string {
	var b strings.Builder

	// Everything around the list takes 7 lines plus the footer, which wraps.
	linesPerPR := 4
	availableHeight := max(m.height-7-footerLines, linesPerPR)
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

func (m model) renderActivePR(p pr.PR, selected bool) string {
	maxTitleWidth := max(m.width-30, 20)
	key := p.RepoShort()

	repoRef := renderRepoRef(p)
	badge := m.queueIndicator(key) + m.badgeFor(key, p.OverallStatus)
	gap := max(m.width-lipgloss.Width(repoRef)-lipgloss.Width(badge)-6, 1)
	line1 := repoRef + strings.Repeat(" ", gap) + badge

	title := truncateDisplayWidth(p.Title, maxTitleWidth)
	line2 := prTitleStyle.Render(title)

	base := "Base " + baseIndicator(p)
	ci := "CI " + ciIndicator(p)
	review := "Review " + reviewIndicator(p)
	auto := m.autoIndicators(p)

	comments := ""
	if p.ReviewThreads > 0 {
		comments = "  Comments " + threadsIndicator(p)
	}

	line3 := prDescStyle.Render(base + "  " + ci + "  " + review + comments + auto)

	content := line1 + "\n" + line2 + "\n" + line3 + "\n"

	if selected {
		return selectedStyle.Render(content)
	}

	return normalStyle.Render(content)
}

func (m model) renderMergedItem(item displayItem, selected bool) string {
	maxTitleWidth := max(m.width-30, 20)

	repoRef := renderRepoRef(item.pr)
	confirmed := false
	merged := false

	if mp := m.RecentlyMerged[item.pr.RepoShort()]; mp != nil {
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

// renderRepoRef renders "owner/repo#123" with the number highlighted, plus the
// original author for PRs taken over from someone else.
func renderRepoRef(p pr.PR) string {
	ref := prRepoStyle.Render(p.Repo+"#") + prNumberStyle.Render(strconv.Itoa(p.Number))
	if p.TakenOverFrom != "" {
		ref += "  " + prTakenOverStyle.Render("⇄ @"+p.TakenOverFrom)
	}

	return ref
}

func (m model) badgeFor(key string, status pr.OverallStatus) string {
	if m.IsMergeFailed(key) {
		return badgeMergeFailed.Render("MERGE FAIL")
	}

	if m.IsRerunFailed(key) {
		return badgeRerunFailed.Render("RERUN FAIL")
	}

	return statusBadge(status)
}

// automergeText says what automerge on the PR is set to do: that it is off,
// what it still waits for while it is on, and the conditions it was given.
func (m model) automergeText(p pr.PR) string {
	key := p.RepoShort()
	name := "automerge"

	if !m.AutoMerge[key] {
		name = "automerge off"
	} else if waits := m.AutomergeWaitsFor(p); len(waits) > 0 {
		return "automerge waits: " + strings.Join(waits, ", ")
	}

	var conditions []string

	if approvals := m.MinApprovals[key]; approvals > 0 {
		conditions = append(conditions, fmt.Sprintf("%d approvals", approvals))
	}

	if m.ResolvedOnly[key] {
		conditions = append(conditions, "comments resolved")
	}

	if len(conditions) == 0 {
		return name
	}

	return name + " (" + strings.Join(conditions, ", ") + ")"
}

func (m model) autoIndicators(p pr.PR) string {
	key := p.RepoShort()

	var parts []string

	if m.IsMergeFailed(key) {
		parts = append(parts, badgeMergeFailed.Render("⚡ merge gave up"))
	} else if m.AutoMerge[key] {
		parts = append(parts, indicatorAuto.Render("⚡ "+m.automergeText(p)))
	} else if text := m.automergeText(p); text != "automerge off" {
		// Conditions set ahead of turning automerge on.
		parts = append(parts, indicatorNone.Render(text))
	}

	if m.IsRerunFailed(key) {
		parts = append(parts, badgeRerunFailed.Render("⚡ rerun gave up"))
	} else if m.AutoRerun[key] {
		parts = append(parts, indicatorAuto.Render("⚡ autorerun"))
	}

	if _, on := m.AutoRebase[key]; on {
		reason, failed := m.rebaseFailure(p)

		switch {
		case failed:
			parts = append(parts, indicatorFail.Render("⚡ rebase failed: "+reason))
		case m.rebasing == key:
			parts = append(parts, indicatorAuto.Render("⚡ rebasing…"))
		default:
			parts = append(parts, indicatorAuto.Render("⚡ autorebase"))
		}
	}

	if len(parts) == 0 {
		return ""
	}

	return "  " + strings.Join(parts, " ")
}

// queueIndicator shows the PR's place in its auto-rebase queue, "▶ 1/3" for
// the active PR and "⏳ 2/3" for one waiting its turn.
func (m model) queueIndicator(key string) string {
	slot, ok := m.queueSlots()[key]
	if !ok {
		return ""
	}

	mark := "⏳"
	if slot.active {
		mark = "▶"
	}

	return indicatorAuto.Render(fmt.Sprintf("%s %d/%d", mark, slot.pos, slot.total)) + "  "
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
	type footerKey struct{ key, desc string }

	if m.help {
		keys := footerKeyStyle.Render("any other key") + " back  ·  " + footerKeyStyle.Render("q") + " quit"
		if m.helpMaxOffset() > 0 {
			keys = footerKeyStyle.Render("j/k") + " scroll  ·  " + keys
		}

		return footerStyle.Render(keys)
	}

	keys := []footerKey{
		{"m", "merge"},
		{"r", "rebase"},
		{"t", "re-run"},
		{"a", "automerge"},
		{"1/2/3", "approvals"},
		{"c", "comments"},
		{"A", "autorerun"},
		{"b", "autorebase"},
		{"J/K", "queue order"},
		{"o", "open"},
		{"R", "refresh"},
		{"?", "help"},
		{"q", "quit"},
	}

	if item, ok := m.selectedItem(); ok && !item.merged && item.pr.AwaitingApprovalRuns > 0 {
		keys = slices.Insert(keys, 3, footerKey{"w", "approve workflows"})
	}

	// The keys wrap onto as many lines as the terminal width needs. On one
	// line the last ones, quit among them, fall off the right edge.
	const separator = "  ·  "

	width := m.width - 2
	if width <= 0 {
		width = math.MaxInt // size not known yet
	}

	var (
		lines []string
		line  string
	)

	for _, k := range keys {
		part := footerKeyStyle.Render(k.key) + " " + k.desc

		switch {
		case line == "":
			line = part
		case lipgloss.Width(line+separator+part) > width:
			lines = append(lines, line)
			line = part
		default:
			line += separator + part
		}
	}

	lines = append(lines, line)

	return footerStyle.Render(strings.Join(lines, "\n"))
}
