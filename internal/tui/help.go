package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/utkuozdemir/gh-pr-lander/internal/hint"
)

// The help view explains every key, every status and every mark on a row. It
// is the terminal's version of the hover texts on the page, and uses the same
// sentences.

type helpEntry struct{ name, text string }

type helpSection struct {
	title   string
	entries []helpEntry
}

var helpSections = []helpSection{
	{"Keys", []helpEntry{
		{"j k", "Move down or up the list. The arrow keys work too."},
		{"g G", "Jump to the first or the last pull request."},
		{"m", hint.Merge},
		{"a", hint.Automerge},
		{"1 2 3", hint.Approvals + " 1 is whatever the repository requires."},
		{"c", hint.CommentsResolved + " Press again to stop waiting."},
		{"A", hint.Autorerun},
		{"t", hint.Rerun},
		{"r", "Rebase onto the base branch here and push. It needs your signing key."},
		{"b", "Rebase this pull request without asking when it falls behind, one at a time per base branch. " +
			"On a failed rebase, press it to try again."},
		{"J K", "Move this pull request down or up its autorebase queue."},
		{"w", "Approve the workflows of a contributor's pull request, so that they run."},
		{"o enter", hint.Open},
		{"R", hint.Refresh},
		{"?", "Show this help."},
		{"q", "Quit."},
	}},
	{"Status", []helpEntry{
		{"READY", hint.Ready},
		{"STALE", hint.Stale},
		{"PENDING", hint.Pending},
		{"FAILED", hint.Failed},
		{"BLOCKED", hint.Blocked},
		{"DRAFT", hint.Draft},
		{"MERGE FAIL", hint.MergeGaveUp},
		{"RERUN FAIL", hint.RerunGaveUp},
		{"CHECKING", hint.Checking},
		{"MERGED", hint.Merged},
		{"CLOSED", hint.Closed},
	}},
	{"On a row", []helpEntry{
		{"Base", hint.BaseBehind + " \"behind (not enforced)\": " + hint.BaseUnenforced},
		{"CI", hint.Checks + " \"optional still running\": " + hint.ChecksOptional +
			" \"workflow awaiting approval\": " + hint.ChecksUnapproved},
		{"Review", hint.Review},
		{"Comments", hint.CommentCount},
		{"1 note", hint.Note},
		{"⇄ @author", hint.TakenOver},
		{"▶ 1/3", hint.QueueActive},
		{"⏳ 2/3", hint.QueueWaiting},
	}},
}

// helpLines lays the help out for the terminal width, one string per screen
// line, so that the view can show a window of it and scroll.
func (m model) helpLines() []string {
	const nameWidth = 12

	textWidth := 60 // size not known yet
	if m.width > 0 {
		textWidth = max(m.width-nameWidth-4, 10)
	}

	text := lipgloss.NewStyle().Width(textWidth)
	name := lipgloss.NewStyle().Width(nameWidth).Bold(true).Foreground(colorWhite)

	var lines []string

	for i, section := range helpSections {
		if i > 0 {
			lines = append(lines, "")
		}

		lines = append(lines, "  "+headerInfoStyle.Render(section.title))

		for _, e := range section.entries {
			row := lipgloss.JoinHorizontal(lipgloss.Top, "  ", name.Render(e.name), text.Render(e.text))
			lines = append(lines, strings.Split(row, "\n")...)
		}
	}

	return lines
}

// helpRoom is how many lines of help fit: the screen minus the header, the
// footer and the blank lines around them. Zero means the size is not known.
func (m model) helpRoom() int {
	if m.height <= 0 {
		return 0
	}

	return max(m.height-5, 3)
}

// helpMaxOffset is how far the help can scroll down.
func (m model) helpMaxOffset() int {
	room := m.helpRoom()
	if room == 0 {
		return 0
	}

	return max(len(m.helpLines())-room, 0)
}

func (m model) renderHelp() string {
	lines := m.helpLines()

	if room := m.helpRoom(); room > 0 && len(lines) > room {
		offset := min(m.helpOffset, len(lines)-room)
		lines = lines[offset : offset+room]
	}

	return strings.Join(lines, "\n") + "\n"
}
