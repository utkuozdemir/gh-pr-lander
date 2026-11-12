package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/utkuozdemir/gh-pr-lander/internal/hint"
)

// The help view explains every key and every status. It is the terminal's
// version of the hover texts on the page, and uses the same sentences.

type helpEntry struct{ name, text string }

var helpKeys = []helpEntry{
	{"m", hint.Merge},
	{"a", hint.Automerge},
	{"1 2 3", hint.Approvals + " 1 is whatever the repository requires."},
	{"c", "Make automerge wait until every review comment is resolved. Press again to stop waiting."},
	{"A", hint.Autorerun},
	{"t", hint.Rerun},
	{"r", "Rebase onto the base branch here and push. It needs your signing key."},
	{"b", "Rebase this pull request without asking when it falls behind. One at a time per base branch."},
	{"J K", "Move this pull request down or up its autorebase queue."},
	{"w", "Approve the workflows of a contributor's pull request, so that they run."},
	{"o", hint.Open},
	{"R", hint.Refresh},
	{"?", "Show or hide this help."},
	{"q", "Quit."},
}

var helpStatuses = []helpEntry{
	{"READY", hint.Ready},
	{"STALE", hint.Stale},
	{"PENDING", hint.Pending},
	{"FAILED", hint.Failed},
	{"BLOCKED", hint.Blocked},
	{"DRAFT", hint.Draft},
	{"MERGE FAIL", hint.MergeGaveUp},
	{"RERUN FAIL", hint.RerunGaveUp},
}

func (m model) renderHelp() string {
	var b strings.Builder

	// Long explanations wrap under themselves, next to the key column.
	const nameWidth = 12

	textWidth := m.width - nameWidth - 4
	if textWidth < 20 {
		textWidth = 60 // size not known yet, or too narrow to wrap sensibly
	}

	text := lipgloss.NewStyle().Width(textWidth)
	name := lipgloss.NewStyle().Width(nameWidth).Bold(true).Foreground(colorWhite)

	section := func(title string, entries []helpEntry) {
		b.WriteString("  " + headerInfoStyle.Render(title) + "\n")

		for _, e := range entries {
			row := lipgloss.JoinHorizontal(lipgloss.Top, "  ", name.Render(e.name), text.Render(e.text))
			b.WriteString(row + "\n")
		}
	}

	section("Keys", helpKeys)
	b.WriteString("\n")
	section("Status", helpStatuses)

	return b.String()
}
