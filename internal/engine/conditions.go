package engine

import (
	"fmt"
	"log/slog"

	"github.com/utkuozdemir/gh-pr-lander/internal/pr"
)

// Automerge conditions.
//
// A PR that is ready can be merged, but the owner may want automerge to wait
// for more: a second or third approval, or every review comment resolved. A
// condition is set per PR and stays with it until the PR is gone, whether
// automerge is on or off. That order matters: turning automerge on for a ready
// PR merges it at once, so the conditions have to be there first. A condition
// holds back automerge only. A merge the owner starts needs no more than
// GitHub does.

// MaxApprovals is the most approvals automerge can be told to wait for.
const MaxApprovals = 3

// AutomergeWaitsFor lists what the PR's automerge conditions still ask for,
// in words for the owner. It is empty when automerge has no condition left to
// wait for, which says nothing about the PR being ready.
func (s *State) AutomergeWaitsFor(p pr.PR) []string {
	key := p.RepoShort()

	var waits []string

	if missing := s.MinApprovals[key] - p.Approvals; missing == 1 {
		waits = append(waits, "1 more approval")
	} else if missing > 1 {
		waits = append(waits, fmt.Sprintf("%d more approvals", missing))
	}

	if s.ResolvedOnly[key] {
		if open := p.UnresolvedThreads(); open == 1 {
			waits = append(waits, "1 unresolved comment")
		} else if open > 1 {
			waits = append(waits, fmt.Sprintf("%d unresolved comments", open))
		}
	}

	return waits
}

// SetMinApprovals tells the PR's automerge how many approvals to wait for.
// One or less means no condition: whatever the repository requires. It
// returns the merge to start if the PR now passes.
func (s *State) SetMinApprovals(p pr.PR, approvals int) (Work, error) {
	key := p.RepoShort()

	if approvals <= 1 {
		delete(s.MinApprovals, key)
	} else {
		s.MinApprovals[key] = min(approvals, MaxApprovals)
	}

	slog.Info("automerge approvals set", "pr", key, "approvals", s.MinApprovals[key])

	return s.conditionChanged(p)
}

// ToggleResolvedOnly flips whether the PR's automerge waits until no review
// comment is unresolved.
func (s *State) ToggleResolvedOnly(p pr.PR) (Work, error) {
	key := p.RepoShort()

	if s.ResolvedOnly[key] {
		delete(s.ResolvedOnly, key)
	} else {
		s.ResolvedOnly[key] = true
	}

	slog.Info("automerge comments condition set", "pr", key, "resolved_only", s.ResolvedOnly[key])

	return s.conditionChanged(p)
}

func (s *State) conditionChanged(p pr.PR) (Work, error) {
	if err := s.SaveDB(); err != nil {
		slog.Error("failed to save db", "error", err)

		return nil, err
	}

	// Loosening a condition can make the PR pass right now.
	return s.autoMergeWork(p), nil
}
