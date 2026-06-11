package tui

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/utkuozdemir/sidero-rebaser/internal/pr"
)

var (
	// Base colors.
	colorGreen   = lipgloss.Color("#22c55e")
	colorYellow  = lipgloss.Color("#eab308")
	colorRed     = lipgloss.Color("#ef4444")
	colorCyan    = lipgloss.Color("#06b6d4")
	colorMagenta = lipgloss.Color("#a855f7")
	colorPurple  = lipgloss.Color("#8b5cf6")
	colorOrange  = lipgloss.Color("#f97316")
	colorGray    = lipgloss.Color("#6b7280")
	colorDim     = lipgloss.Color("#4b5563")
	colorWhite   = lipgloss.Color("#f9fafb")

	// Header.
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorWhite).
			PaddingLeft(1)

	headerInfoStyle = lipgloss.NewStyle().
			Foreground(colorGray)

	// PR item.
	selectedStyle = lipgloss.NewStyle().
			PaddingLeft(1).
			BorderLeft(true).
			BorderStyle(lipgloss.ThickBorder()).
			BorderForeground(colorCyan)

	normalStyle = lipgloss.NewStyle().
			PaddingLeft(2)

	prTitleStyle = lipgloss.NewStyle().
			Bold(true)

	prRepoStyle = lipgloss.NewStyle().
			Foreground(colorGray)

	prDescStyle = lipgloss.NewStyle().
			Foreground(colorDim)

	// Status badges.
	badgeReady = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorGreen)

	badgeStale = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorYellow)

	badgePending = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorCyan)

	badgeFailed = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorMagenta)

	badgeBlocked = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorRed)

	badgeDraft = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorGray)

	badgeMergeFailed = lipgloss.NewStyle().
				Bold(true).
				Foreground(colorOrange)

	badgeRerunFailed = lipgloss.NewStyle().
				Bold(true).
				Foreground(colorOrange)

	badgeMerged = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorPurple)

	// Indicators.
	indicatorPass    = lipgloss.NewStyle().Foreground(colorGreen)
	indicatorWarn    = lipgloss.NewStyle().Foreground(colorYellow)
	indicatorFail    = lipgloss.NewStyle().Foreground(colorRed)
	indicatorRunning = lipgloss.NewStyle().Foreground(colorCyan)
	indicatorNone    = lipgloss.NewStyle().Foreground(colorGray)
	indicatorAuto    = lipgloss.NewStyle().Foreground(colorOrange)

	// Footer.
	footerStyle = lipgloss.NewStyle().
			Foreground(colorGray).
			PaddingLeft(1)

	footerKeyStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorWhite)

	// Messages.
	successStyle = lipgloss.NewStyle().
			Foreground(colorGreen)

	errorStyle = lipgloss.NewStyle().
			Foreground(colorRed)
)

func statusBadge(status pr.OverallStatus) string {
	switch status {
	case pr.StatusReady:
		return badgeReady.Render("READY")
	case pr.StatusStale:
		return badgeStale.Render("STALE")
	case pr.StatusPending:
		return badgePending.Render("PENDING")
	case pr.StatusFailed:
		return badgeFailed.Render("FAILED")
	case pr.StatusBlocked:
		return badgeBlocked.Render("BLOCKED")
	case pr.StatusDraft:
		return badgeDraft.Render("DRAFT")
	default:
		return ""
	}
}

func baseIndicator(status pr.BaseStatus) string {
	switch status {
	case pr.BaseSynced:
		return indicatorPass.Render("✓ synced")
	case pr.BaseBehind:
		return indicatorWarn.Render("↻ behind")
	case pr.BaseConflict:
		return indicatorFail.Render("✗ conflict")
	case pr.BaseUnknown:
		return indicatorNone.Render("? checking")
	default:
		return ""
	}
}

func ciIndicator(status pr.CIStatus, checks pr.CheckSummary) string {
	switch status {
	case pr.CIPassed:
		return indicatorPass.Render("✓ " + checks.String())
	case pr.CIRunning:
		return indicatorRunning.Render("● " + checks.String())
	case pr.CIFailed:
		return indicatorFail.Render("✗ " + checks.String())
	case pr.CINone:
		return indicatorNone.Render("— no checks")
	default:
		return ""
	}
}

func reviewIndicator(status pr.ReviewStatus) string {
	switch status {
	case pr.ReviewApproved:
		return indicatorPass.Render("✓ approved")
	case pr.ReviewChangesRequested:
		return indicatorFail.Render("✗ changes requested")
	case pr.ReviewPending:
		return indicatorWarn.Render("◯ pending")
	case pr.ReviewNone:
		return indicatorNone.Render("— none")
	default:
		return ""
	}
}
