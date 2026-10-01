package tui

import (
	"log/slog"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/utkuozdemir/sidero-rebaser/internal/notify"
	"github.com/utkuozdemir/sidero-rebaser/internal/pr"
)

// detectMerged finds PRs that disappeared since the last fetch, records them as
// recently merged/closed, and schedules a check (and optional branch cleanup).
func (m model) detectMerged() []tea.Cmd {
	if len(m.prevPRs) == 0 {
		return nil
	}

	currentKeys := make(map[string]bool, len(m.prs))
	for _, p := range m.prs {
		currentKeys[p.RepoShort()] = true
	}

	var cmds []tea.Cmd

	for _, p := range m.prevPRs {
		key := p.RepoShort()
		if !currentKeys[key] && m.recentlyMerged[key] == nil {
			slog.Info("PR disappeared (merged/closed)", "pr", key)
			m.recentlyMerged[key] = &mergedPR{PR: p, GoneAt: time.Now()}

			deleteBranch := m.config.DeleteMergedBranches &&
				p.HeadRepo != "" && p.HeadBranch != "" &&
				p.HeadRepo != p.Repo && // only delete on forks, never same-repo branches
				strings.EqualFold(p.HeadRepoOwner, m.client.Username())
			cmds = append(cmds, checkMergedPRCmd(m.client, p, deleteBranch))
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
	for _, p := range m.prs {
		openPRs[p.RepoShort()] = true
	}

	changed := false

	for key := range m.autoMerge {
		if !openPRs[key] && m.isConfirmedGone(key) {
			delete(m.autoMerge, key)

			changed = true
		}
	}

	for key := range m.autoRebase {
		if !openPRs[key] && m.isConfirmedGone(key) {
			delete(m.autoRebase, key)
			delete(m.rebaseState, key)

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

// checkTransitions fires desktop notifications for meaningful status changes
// between the previous and current fetch. A change that automation on the PR
// is about to handle stays quiet: only automation's outcome notifies.
func (m model) checkTransitions() {
	if len(m.prevPRs) == 0 {
		return
	}

	prevMap := make(map[string]pr.PR)
	for _, p := range m.prevPRs {
		prevMap[p.RepoShort()] = p
	}

	for _, p := range m.prs {
		prev, exists := prevMap[p.RepoShort()]
		if !exists {
			continue
		}

		key := p.RepoShort()
		body := key + " — " + p.Title
		_, autoRebase := m.autoRebase[key]

		if prev.OverallStatus != pr.StatusReady && p.OverallStatus == pr.StatusReady {
			slog.Info("PR became ready to merge", "pr", key)

			if !m.autoMerge[key] || m.mergeFailed[key] {
				notify.Send(notify.ReadyToMerge, body)
			}
		}

		if prev.CIStatus != pr.CIFailed && p.CIStatus == pr.CIFailed {
			slog.Info("checks failed", "pr", key)

			if !m.autorerunWillHandle(p) {
				notify.Send(notify.ChecksFailed, body)
			}
		}

		if prev.BaseStatus == pr.BaseSynced && p.BaseStatus == pr.BaseBehind {
			slog.Info("PR became stale", "pr", key)

			if !autoRebase {
				notify.Send(notify.NeedsRebase, body)
			}
		}
	}
}
