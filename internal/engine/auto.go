package engine

import (
	"time"

	"github.com/utkuozdemir/sidero-rebaser/internal/pr"
)

// Automerge and autorerun decisions, and the two settling guards.

func (s *State) IsMergeFailed(key string) bool {
	return s.AutoMerge[key] && s.MergeFailed[key]
}

func (s *State) IsRerunFailed(key string) bool {
	return s.AutoRerun[key] && s.RerunFailed[key]
}

// AutoWorks returns the merges and re-runs that automation should start now.
func (s *State) AutoWorks() []Work {
	var works []Work

	for _, p := range s.PRs {
		if w := s.autoMergeWork(p); w != nil {
			works = append(works, w)
		}

		if w := s.autoRerunWork(p); w != nil {
			works = append(works, w)
		}
	}

	return works
}

func (s *State) autoMergeWork(p pr.PR) Work {
	key := p.RepoShort()
	if !s.shouldAutoMerge(p) || !s.markAutoCheck("merge", key) {
		return nil
	}

	return s.MergeWork(p, true)
}

func (s *State) autoRerunWork(p pr.PR) Work {
	key := p.RepoShort()
	if !s.shouldAutoRerun(p) || !s.markAutoCheck("rerun", key) {
		return nil
	}

	return s.autoRerunCheckWork(p)
}

func (s *State) shouldAutoMerge(p pr.PR) bool {
	key := p.RepoShort()

	return s.AutoMerge[key] && !s.MergeFailed[key] && p.OverallStatus == pr.StatusReady
}

func (s *State) shouldAutoRerun(p pr.PR) bool {
	key := p.RepoShort()
	if !s.AutoRerun[key] || s.RerunFailed[key] || p.CIStatus != pr.CIFailed ||
		!p.FailedWorkflowRunsKnown || p.FailedWorkflowRuns <= 0 {
		return false
	}

	last := s.LastAutoRerun[key]

	return last.IsZero() || time.Since(last) >= s.Config.AutoCooldown
}

// AutorerunWillHandle reports whether autorerun is going to re-run the PR's
// failed checks: it is on, has not given up, and an Actions run on the head
// failed or is still finishing. A failure outside Actions is never re-run.
func (s *State) AutorerunWillHandle(p pr.PR) bool {
	key := p.RepoShort()

	return s.AutoRerun[key] && !s.RerunFailed[key] && p.FailedWorkflowRunsKnown &&
		(p.FailedWorkflowRuns > 0 || p.RunningWorkflowRuns > 0)
}

func (s *State) markAutoCheck(kind, key string) bool {
	if s.LastAutoCheck == nil {
		s.LastAutoCheck = make(map[string]time.Time)
	}

	checkKey := kind + ":" + key
	if time.Since(s.LastAutoCheck[checkKey]) < s.Config.AutoCooldown {
		return false
	}

	s.LastAutoCheck[checkKey] = time.Now()

	return true
}

func (s *State) clearAutoCheck(kind Kind, key string) {
	switch kind {
	case Automerge:
		delete(s.LastAutoCheck, "merge:"+key)
	case Autorerun:
		delete(s.LastAutoCheck, "rerun:"+key)
	}
}

// Rerun settling (issue #1).
//
// After a re-run is triggered, GitHub's check rollup can briefly drop the
// resetting checks and report "all passed", which would make a failed PR look
// READY and trigger a spurious merge. We mark such PRs in flight and, while the
// settle grace is active, treat any transient passed/none state as still
// running. We stop settling as soon as GitHub catches up (CI shows running
// again), the head SHA changes (a new push), or the grace window elapses.

func (s *State) MarkRerunInFlight(key, headSHA string) {
	if s.RerunInFlight == nil {
		s.RerunInFlight = make(map[string]time.Time)
		s.RerunInFlightSHA = make(map[string]string)
	}

	s.RerunInFlight[key] = time.Now()
	s.RerunInFlightSHA[key] = headSHA
}

func (s *State) clearRerunInFlight(key string) {
	delete(s.RerunInFlight, key)
	delete(s.RerunInFlightSHA, key)
}

// applyRerunSettling rewrites the freshly fetched PRs so that any PR with a
// recent in-flight re-run is not prematurely considered ready. It also prunes
// in-flight entries that no longer apply. It mutates the in-flight maps, hence
// the pointer receiver.
func (s *State) applyRerunSettling(prs []pr.PR) []pr.PR {
	if len(s.RerunInFlight) == 0 {
		return prs
	}

	grace := s.Config.RerunSettleGrace()
	present := make(map[string]bool, len(prs))

	for i := range prs {
		key := prs[i].RepoShort()

		triggered, ok := s.RerunInFlight[key]
		if !ok {
			continue
		}

		present[key] = true

		// A new push invalidates the old run entirely.
		if sha := s.RerunInFlightSHA[key]; sha != "" && prs[i].HeadSHA != "" && sha != prs[i].HeadSHA {
			s.clearRerunInFlight(key)

			continue
		}

		// GitHub has caught up: the re-run is genuinely running again. The
		// transient passed window is over, so stop guarding.
		if prs[i].CIStatus == pr.CIRunning {
			s.clearRerunInFlight(key)

			continue
		}

		// Grace elapsed — give up guarding and trust GitHub.
		if time.Since(triggered) >= grace {
			s.clearRerunInFlight(key)

			continue
		}

		// Still settling. A "passed"/"none" reading here is suspect (the failed
		// checks were just reset), so treat it as running to keep the PR out of
		// READY and block a spurious merge.
		if prs[i].CIStatus == pr.CIPassed || prs[i].CIStatus == pr.CINone {
			prs[i].CIStatus = pr.CIRunning
			prs[i].OverallStatus = pr.ComputeOverallStatus(prs[i])
		}
	}

	// Prune in-flight entries for PRs that vanished from the fetch (merged or
	// closed) so the maps do not grow unbounded.
	for key := range s.RerunInFlight {
		if !present[key] {
			if time.Since(s.RerunInFlight[key]) >= grace {
				s.clearRerunInFlight(key)
			}
		}
	}

	return prs
}

// Checks settling (issue #7).
//
// When a branch is rebased and force-pushed outside the tool, GitHub creates a
// new head commit and re-runs CI, but the new commit's checks register over a
// few seconds. During that window the check rollup can report a small, all-green
// subset (sometimes a single passing check), which makes the PR look READY and
// could fire a spurious merge. Unlike the rerun guard, no in-tool action arms here,
// so we lean on a cross-fetch signal: if a PR's head SHA just changed and its
// current check count is below what the previous commit ultimately had, the new
// commit's checks have not all registered yet, so a transient passed/none
// reading is treated as still running. The guard relaxes once the checks
// re-register (the count catches up) or a short grace elapses.
//
// The same signal on one commit covers a re-run started outside this process:
// the reset checks leave the rollup for a moment, the count falls below what
// the commit already had, and what is left can be all green.

func (s *State) applyChecksSettling(prs []pr.PR) []pr.PR {
	if s.CheckBaseline == nil {
		s.CheckBaseline = make(map[string]CheckBaseline)
	}

	now := time.Now()
	grace := s.Config.RerunSettleGrace()
	present := make(map[string]bool, len(prs))

	for i := range prs {
		p := &prs[i]
		if p.HeadSHA == "" {
			continue
		}

		key := p.RepoShort()
		present[key] = true

		bl, ok := s.CheckBaseline[key]

		switch {
		case !ok:
			// First sighting: nothing to compare against yet.
			bl = CheckBaseline{SHA: p.HeadSHA, FirstSeen: now, MaxTotal: p.Checks.Total}
		case bl.SHA != p.HeadSHA:
			// A new commit. Carry the previous commit's high-water check count
			// forward as the baseline the new (still-registering) checks must
			// catch up to.
			bl = CheckBaseline{SHA: p.HeadSHA, FirstSeen: now, MaxTotal: p.Checks.Total, PrevMaxTotal: bl.MaxTotal}
		default:
			// Same commit: checks only grow as they register, so keep the
			// high-water mark. A count below it means checks were reset by a
			// re-run this process did not start (GitHub's page, another
			// instance of the tool), so the rerun guard above is not armed.
			switch {
			case p.Checks.Total >= bl.MaxTotal:
				bl.MaxTotal = p.Checks.Total
				bl.DroppedAt = time.Time{}
			case bl.DroppedAt.IsZero():
				bl.DroppedAt = now
			}
		}

		s.CheckBaseline[key] = bl

		// Suppress only a freshly-pushed commit that currently looks all-green
		// while still showing fewer checks than its predecessor had — i.e. the
		// required checks have not registered yet.
		registering := bl.PrevMaxTotal > p.Checks.Total && now.Sub(bl.FirstSeen) < grace
		reset := !bl.DroppedAt.IsZero() && now.Sub(bl.DroppedAt) < grace

		if (p.CIStatus == pr.CIPassed || p.CIStatus == pr.CINone) && (registering || reset) {
			p.CIStatus = pr.CIRunning
			p.OverallStatus = pr.ComputeOverallStatus(*p)
		}
	}

	// Prune baselines for PRs that have left the fetch (merged or closed) once
	// they are past the grace, so the map does not grow unbounded.
	for key, bl := range s.CheckBaseline {
		if !present[key] && now.Sub(bl.FirstSeen) >= grace {
			delete(s.CheckBaseline, key)
		}
	}

	return prs
}
