package engine

import (
	"log/slog"
	"strings"
	"time"

	"github.com/utkuozdemir/gh-pr-lander/internal/notify"
	"github.com/utkuozdemir/gh-pr-lander/internal/pr"
)

// detectMerged finds PRs that disappeared since the last fetch, records them as
// recently merged/closed, and returns a check (and optional branch cleanup) for
// each.
func (s *State) detectMerged() []Work {
	if len(s.PrevPRs) == 0 {
		return nil
	}

	currentKeys := make(map[string]bool, len(s.PRs))
	for _, p := range s.PRs {
		currentKeys[p.RepoShort()] = true
	}

	var works []Work

	for _, p := range s.PrevPRs {
		key := p.RepoShort()
		if !currentKeys[key] && s.RecentlyMerged[key] == nil {
			slog.Info("PR disappeared (merged/closed)", "pr", key)
			s.RecentlyMerged[key] = &MergedPR{PR: p, GoneAt: time.Now()}

			deleteBranch := s.Config.DeleteMergedBranches &&
				p.HeadRepo != "" && p.HeadBranch != "" &&
				p.HeadRepo != p.Repo && // only delete on forks, never same-repo branches
				strings.EqualFold(p.HeadRepoOwner, s.Client.Username())
			works = append(works, s.mergedCheckWork(p, deleteBranch))
		}
	}

	for key, mp := range s.RecentlyMerged {
		if time.Since(mp.GoneAt) >= s.Config.MergedDisplayDuration {
			delete(s.RecentlyMerged, key)
		}
	}

	return works
}

// cleanupAuto drops the switches of PRs that are confirmed gone.
func (s *State) cleanupAuto() error {
	openPRs := make(map[string]bool, len(s.PRs))
	for _, p := range s.PRs {
		openPRs[p.RepoShort()] = true
	}

	changed := false

	for key := range s.AutoMerge {
		if !openPRs[key] && s.isConfirmedGone(key) {
			delete(s.AutoMerge, key)

			changed = true
		}
	}

	// The conditions are set before automerge is turned on, so they can be
	// there without it.
	for key := range s.MinApprovals {
		if !openPRs[key] && s.isConfirmedGone(key) {
			delete(s.MinApprovals, key)

			changed = true
		}
	}

	for key := range s.ResolvedOnly {
		if !openPRs[key] && s.isConfirmedGone(key) {
			delete(s.ResolvedOnly, key)

			changed = true
		}
	}

	for key := range s.AutoRebase {
		if !openPRs[key] && s.isConfirmedGone(key) {
			delete(s.AutoRebase, key)

			changed = true
		}
	}

	for key := range s.AutoRerun {
		if !openPRs[key] && s.isConfirmedGone(key) {
			delete(s.AutoRerun, key)
			delete(s.LastAutoRerun, key)

			changed = true
		}
	}

	if !changed {
		return nil
	}

	if err := s.SaveDB(); err != nil {
		slog.Error("failed to save db", "error", err)

		return err
	}

	return nil
}

func (s *State) isConfirmedGone(key string) bool {
	mp := s.RecentlyMerged[key]

	return mp != nil && mp.Confirmed
}

// checkTransitions fires desktop notifications for meaningful status changes
// between the previous and current fetch. A change that automation on the PR
// is about to handle stays quiet: only automation's outcome notifies.
func (s *State) checkTransitions() {
	if len(s.PrevPRs) == 0 {
		return
	}

	prevMap := make(map[string]pr.PR)
	for _, p := range s.PrevPRs {
		prevMap[p.RepoShort()] = p
	}

	for _, p := range s.PRs {
		prev, exists := prevMap[p.RepoShort()]
		if !exists {
			continue
		}

		key := p.RepoShort()
		body := key + " — " + p.Title
		_, autoRebase := s.AutoRebase[key]

		// Giving up is about one commit: a refused merge, or re-runs that
		// kept failing. A new head gets a new try.
		if prev.HeadSHA != p.HeadSHA {
			delete(s.MergeFailed, key)
			delete(s.RerunFailed, key)
			s.clearAutoCheck(Automerge, key)
			s.clearAutoCheck(Autorerun, key)
		}

		if prev.OverallStatus != pr.StatusReady && p.OverallStatus == pr.StatusReady {
			slog.Info("PR became ready to merge", "pr", key)

			if !s.AutoMerge[key] || s.MergeFailed[key] {
				notify.Send(notify.ReadyToMerge, body)
			}
		}

		if prev.CIStatus != pr.CIFailed && p.CIStatus == pr.CIFailed {
			slog.Info("checks failed", "pr", key)

			if !s.AutorerunWillHandle(p) {
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

// confirmGone records the API-confirmed outcome of a PR that left the list and
// notifies about it.
func (s *State) confirmGone(key string, merged bool) {
	mp := s.RecentlyMerged[key]
	if mp == nil {
		return
	}

	mp.Confirmed = true
	mp.Merged = merged

	event := notify.Closed
	if merged {
		event = notify.Merged
	}

	notify.Send(event, key+" — "+mp.PR.Title)
}

// MergedCheckDone records what the check of a PR that left the list found.
func (s *State) MergedCheckDone(res MergedCheckResult) {
	switch {
	case res.Err != nil:
		slog.Error("merged PR check/cleanup error", "pr", res.PRKey, "error", res.Err)

		// Only the branch cleanup failed, the merge itself is confirmed.
		if res.Merged {
			s.confirmGone(res.PRKey, true)
		}
	case res.Open:
		delete(s.RecentlyMerged, res.PRKey)
		slog.Info("PR is still open, keeping it out of merged/closed list", "pr", res.PRKey)
	case res.Deleted:
		s.confirmGone(res.PRKey, true)
		slog.Info("deleted merged branch", "pr", res.PRKey, "branch", res.Branch)
	case !res.Merged:
		s.confirmGone(res.PRKey, false)
		slog.Info("PR closed without merge", "pr", res.PRKey)
	default:
		s.confirmGone(res.PRKey, true)
	}
}
