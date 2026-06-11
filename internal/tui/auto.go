package tui

import (
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/utkuozdemir/sidero-rebaser/internal/pr"
)

// Auto-merge / auto-rerun logic and the rerun "settling" guard for issue #1.

func (m model) isMergeFailed(key string) bool {
	return m.autoMerge[key] && m.mergeFailed[key]
}

func (m model) isRerunFailed(key string) bool {
	return m.autoRerun[key] && m.rerunFailed[key]
}

func (m *model) autoCmds() []tea.Cmd {
	var cmds []tea.Cmd

	for _, p := range m.prs {
		if cmd := m.autoMergeCmd(p); cmd != nil {
			cmds = append(cmds, cmd)
		}

		if cmd := m.autoRerunCmd(p); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}

	return cmds
}

func (m *model) immediateAutoCmd(p pr.PR, label string) tea.Cmd {
	switch label {
	case "automerge":
		return m.autoMergeCmd(p)
	case "autorerun":
		return m.autoRerunCmd(p)
	default:
		return nil
	}
}

func (m *model) autoMergeCmd(p pr.PR) tea.Cmd {
	key := p.RepoShort()
	if !m.shouldAutoMerge(p) || !m.markAutoCheck("merge", key) {
		return nil
	}

	return autoMergeCmd(m.client, p, m.config)
}

func (m *model) autoRerunCmd(p pr.PR) tea.Cmd {
	key := p.RepoShort()
	if !m.shouldAutoRerun(p) || !m.markAutoCheck("rerun", key) {
		return nil
	}

	return autoRerunCmd(m.client, p, m.config.AutoMaxRetries)
}

func (m model) shouldAutoMerge(p pr.PR) bool {
	key := p.RepoShort()

	return m.autoMerge[key] && !m.mergeFailed[key] && p.OverallStatus == pr.StatusReady
}

func (m model) shouldAutoRerun(p pr.PR) bool {
	key := p.RepoShort()
	if !m.autoRerun[key] || m.rerunFailed[key] || p.CIStatus != pr.CIFailed ||
		!p.FailedWorkflowRunsKnown || p.FailedWorkflowRuns <= 0 {
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

// Rerun settling (issue #1).
//
// After a re-run is triggered, GitHub's check rollup can briefly drop the
// resetting checks and report "all passed", which would make a failed PR look
// READY and trigger a spurious /m. We mark such PRs in flight and, while the
// settle grace is active, treat any transient passed/none state as still
// running. We stop settling as soon as GitHub catches up (CI shows running
// again), the head SHA changes (a new push), or the grace window elapses.

func (m *model) markRerunInFlight(key, headSHA string) {
	if m.rerunInFlight == nil {
		m.rerunInFlight = make(map[string]time.Time)
		m.rerunInFlightSHA = make(map[string]string)
	}

	m.rerunInFlight[key] = time.Now()
	m.rerunInFlightSHA[key] = headSHA
}

func (m *model) clearRerunInFlight(key string) {
	delete(m.rerunInFlight, key)
	delete(m.rerunInFlightSHA, key)
}

// applyRerunSettling rewrites the freshly fetched PRs so that any PR with a
// recent in-flight re-run is not prematurely considered ready. It also prunes
// in-flight entries that no longer apply. It mutates the in-flight maps, hence
// the pointer receiver.
func (m *model) applyRerunSettling(prs []pr.PR) []pr.PR {
	if len(m.rerunInFlight) == 0 {
		return prs
	}

	grace := m.config.RerunSettleGrace()
	present := make(map[string]bool, len(prs))

	for i := range prs {
		key := prs[i].RepoShort()

		triggered, ok := m.rerunInFlight[key]
		if !ok {
			continue
		}

		present[key] = true

		// A new push invalidates the old run entirely.
		if sha := m.rerunInFlightSHA[key]; sha != "" && prs[i].HeadSHA != "" && sha != prs[i].HeadSHA {
			m.clearRerunInFlight(key)

			continue
		}

		// GitHub has caught up: the re-run is genuinely running again. The
		// transient passed window is over, so stop guarding.
		if prs[i].CIStatus == pr.CIRunning {
			m.clearRerunInFlight(key)

			continue
		}

		// Grace elapsed — give up guarding and trust GitHub.
		if time.Since(triggered) >= grace {
			m.clearRerunInFlight(key)

			continue
		}

		// Still settling. A "passed"/"none" reading here is suspect (the failed
		// checks were just reset), so treat it as running to keep the PR out of
		// READY and block a spurious /m.
		if prs[i].CIStatus == pr.CIPassed || prs[i].CIStatus == pr.CINone {
			prs[i].CIStatus = pr.CIRunning
			prs[i].OverallStatus = pr.ComputeOverallStatus(prs[i])
		}
	}

	// Prune in-flight entries for PRs that vanished from the fetch (merged or
	// closed) so the maps do not grow unbounded.
	for key := range m.rerunInFlight {
		if !present[key] {
			if time.Since(m.rerunInFlight[key]) >= grace {
				m.clearRerunInFlight(key)
			}
		}
	}

	return prs
}
