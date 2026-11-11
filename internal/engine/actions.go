package engine

import (
	"log/slog"
	"time"

	"github.com/utkuozdemir/sidero-rebaser/internal/pr"
)

// Kind names one of the two automation switches a PR has. Its value is the
// word shown to the user.
type Kind string

const (
	Automerge Kind = "automerge"
	Autorerun Kind = "autorerun"
)

// RefusedError says why an action the user asked for is not started. A soft one is a
// "not now" and reads as a warning, the rest are errors.
type RefusedError struct {
	Reason string
	Soft   bool
}

func (r *RefusedError) Error() string { return r.Reason }

// CheckMerge reports whether the user may merge the PR now.
func (s *State) CheckMerge(p pr.PR) error {
	if p.OverallStatus != pr.StatusReady {
		return &RefusedError{Reason: "PR not ready to merge — need synced base + passing CI"}
	}

	return nil
}

// StartRerun reports whether the user may re-run the PR's failed jobs now, and
// if so starts the cooldown that keeps a second re-run from following at once.
func (s *State) StartRerun(p pr.PR) error {
	if p.CIStatus != pr.CIFailed {
		return &RefusedError{Reason: "No failed checks to re-run"}
	}

	if p.FailedWorkflowRunsKnown && p.FailedWorkflowRuns == 0 {
		if p.RunningWorkflowRuns > 0 {
			return &RefusedError{
				Reason: "A workflow is still running. If the failure is in it, failed jobs can be re-run " +
					"once it finishes, or turn on autorerun",
				Soft: true,
			}
		}

		return &RefusedError{Reason: "No failed GitHub Actions jobs to re-run"}
	}

	key := p.RepoShort()
	if last, ok := s.LastRerun[key]; ok && time.Since(last) < time.Minute {
		return &RefusedError{Reason: "Already re-ran checks less than a minute ago", Soft: true}
	}

	s.LastRerun[key] = time.Now()

	return nil
}

// Toggle flips one automation switch of the PR and saves it. When the switch
// went on and there is something to do right away, it returns that work.
func (s *State) Toggle(kind Kind, p pr.PR) (enabled bool, work Work, err error) {
	entries, failed := s.AutoMerge, s.MergeFailed
	if kind == Autorerun {
		entries, failed = s.AutoRerun, s.RerunFailed
	}

	key := p.RepoShort()

	if entries[key] {
		delete(entries, key)
		delete(failed, key)
		s.clearAutoCheck(kind, key)

		if kind == Autorerun {
			delete(s.LastAutoRerun, key)
		}

		slog.Info(string(kind)+" disabled", "pr", key)
	} else {
		entries[key] = true
		enabled = true

		slog.Info(string(kind)+" enabled", "pr", key)
	}

	if err := s.SaveDB(); err != nil {
		slog.Error("failed to save db", "error", err)

		return enabled, nil, err
	}

	if !enabled {
		return false, nil, nil
	}

	if kind == Autorerun {
		return true, s.autoRerunWork(p), nil
	}

	return true, s.autoMergeWork(p), nil
}
