package main

import "github.com/charmbracelet/lipgloss"

var (
	// Base colors
	colorGreen   = lipgloss.Color("#22c55e")
	colorYellow  = lipgloss.Color("#eab308")
	colorRed     = lipgloss.Color("#ef4444")
	colorCyan    = lipgloss.Color("#06b6d4")
	colorMagenta = lipgloss.Color("#a855f7")
	colorOrange  = lipgloss.Color("#f97316")
	colorGray    = lipgloss.Color("#6b7280")
	colorDim     = lipgloss.Color("#4b5563")
	colorWhite   = lipgloss.Color("#f9fafb")

	// Header
	headerStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorWhite).
			PaddingLeft(1)

	headerInfoStyle = lipgloss.NewStyle().
			Foreground(colorGray)

	// PR item
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

	// Status badges
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

	// Indicators
	indicatorPass    = lipgloss.NewStyle().Foreground(colorGreen)
	indicatorWarn    = lipgloss.NewStyle().Foreground(colorYellow)
	indicatorFail    = lipgloss.NewStyle().Foreground(colorRed)
	indicatorRunning = lipgloss.NewStyle().Foreground(colorCyan)
	indicatorNone    = lipgloss.NewStyle().Foreground(colorGray)
	indicatorAuto    = lipgloss.NewStyle().Foreground(colorOrange)

	// Footer
	footerStyle = lipgloss.NewStyle().
			Foreground(colorGray).
			PaddingLeft(1)

	footerKeyStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(colorWhite)

	// Messages
	successStyle = lipgloss.NewStyle().
			Foreground(colorGreen)

	errorStyle = lipgloss.NewStyle().
			Foreground(colorRed)

)

func statusBadge(status OverallStatus) string {
	switch status {
	case StatusReady:
		return badgeReady.Render("READY")
	case StatusStale:
		return badgeStale.Render("STALE")
	case StatusPending:
		return badgePending.Render("PENDING")
	case StatusFailed:
		return badgeFailed.Render("FAILED")
	case StatusBlocked:
		return badgeBlocked.Render("BLOCKED")
	case StatusDraft:
		return badgeDraft.Render("DRAFT")
	default:
		return ""
	}
}

func baseIndicator(status BaseStatus) string {
	switch status {
	case BaseSynced:
		return indicatorPass.Render("✓ synced")
	case BaseBehind:
		return indicatorWarn.Render("↻ behind")
	case BaseConflict:
		return indicatorFail.Render("✗ conflict")
	case BaseUnknown:
		return indicatorNone.Render("? checking")
	default:
		return ""
	}
}

func ciIndicator(status CIStatus, checks CheckSummary) string {
	switch status {
	case CIPassed:
		return indicatorPass.Render("✓ " + checks.String())
	case CIRunning:
		return indicatorRunning.Render("● " + checks.String())
	case CIFailed:
		return indicatorFail.Render("✗ " + checks.String())
	case CINone:
		return indicatorNone.Render("— no checks")
	default:
		return ""
	}
}

func reviewIndicator(status ReviewStatus) string {
	switch status {
	case ReviewApproved:
		return indicatorPass.Render("✓ approved")
	case ReviewChangesRequested:
		return indicatorFail.Render("✗ changes requested")
	case ReviewPending:
		return indicatorWarn.Render("◯ pending")
	case ReviewNone:
		return indicatorNone.Render("— none")
	default:
		return ""
	}
}
