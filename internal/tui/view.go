package tui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/utkuozdemir/sidero-rebaser/internal/pr"
)

func (m model) View() string {
	if m.quitting {
		return ""
	}

	var b strings.Builder

	b.WriteString(m.renderHeader())
	b.WriteString("\n\n")

	items := m.displayItems()

	switch {
	case m.loading && len(items) == 0:
		b.WriteString("  " + m.spinner.View() + " Loading PRs...")
		b.WriteString("\n")
	case m.err != nil && len(items) == 0:
		b.WriteString(errorStyle.Render("  Error: " + m.err.Error()))
		b.WriteString("\n")
	case len(items) == 0:
		b.WriteString("  No open PRs found.\n")
	default:
		b.WriteString(m.renderItemList(items))
	}

	if m.status != "" {
		b.WriteString("\n")
		b.WriteString("  " + m.status)
		b.WriteString("\n")
	}

	if m.err != nil && len(m.PRs) > 0 {
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
	if len(m.PRs) > 0 {
		prCount = fmt.Sprintf(" · %d PRs", len(m.PRs))
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
	review := "Review " + reviewIndicator(p.ReviewStatus)
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

func (m model) autoIndicators(p pr.PR) string {
	key := p.RepoShort()

	var parts []string

	if m.IsMergeFailed(key) {
		parts = append(parts, badgeMergeFailed.Render("⚡ merge gave up"))
	} else if m.AutoMerge[key] {
		parts = append(parts, indicatorAuto.Render("⚡ automerge"))
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

	keys := []footerKey{
		{"m", "merge"},
		{"r", "rebase"},
		{"t", "re-run"},
		{"a", "automerge"},
		{"A", "autorerun"},
		{"b", "autorebase"},
		{"J/K", "queue order"},
		{"o", "open"},
		{"R", "refresh"},
		{"q", "quit"},
	}

	if item, ok := m.selectedItem(); ok && !item.merged && item.pr.AwaitingApprovalRuns > 0 {
		keys = slices.Insert(keys, 3, footerKey{"w", "approve workflows"})
	}

	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = footerKeyStyle.Render(k.key) + " " + k.desc
	}

	return footerStyle.Render(strings.Join(parts, "  ·  "))
}
