package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/utkuozdemir/sidero-rebaser/internal/github"
	"github.com/utkuozdemir/sidero-rebaser/internal/notify"
	"github.com/utkuozdemir/sidero-rebaser/internal/pr"
)

// Work is one blocking round of GitHub calls. It touches no State, so it can
// run anywhere. Its Result goes back into the matching Done method.
type Work func() Result

// Result is one of MergeResult, RerunResult, AutoRerunResult or
// MergedCheckResult.
type Result any

// MergeResult is the outcome of a fast-forward merge, started by the user or by
// automerge (Auto).
type MergeResult struct {
	PR   pr.PR
	Auto bool
	Err  error
}

// RerunResult is the outcome of a re-run of failed jobs started by the user.
type RerunResult struct {
	PR  pr.PR
	Err error
}

// AutoRerunResult is the outcome of one autorerun attempt.
type AutoRerunResult struct {
	PRKey       string
	HeadSHA     string
	Reran       bool
	Failed      bool // max retries exceeded
	Unavailable bool // no failed GitHub Actions runs exist for this SHA
	ReranAt     time.Time
	Err         error
}

// MergedCheckResult says what became of a PR that left the list.
type MergedCheckResult struct {
	PRKey   string
	Branch  string
	Open    bool // true if the PR was omitted from search but is still open
	Merged  bool // true if API confirmed merged, false if closed without merge
	Deleted bool // true if branch was actually deleted
	Err     error
}

// MergeWork fast-forward merges the PR at the head commit it has in p.
func (s *State) MergeWork(p pr.PR, auto bool) Work {
	client := s.Client

	return func() Result {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		err := client.FastForwardMerge(ctx, p.Repo, p.BaseBranch, p.HeadSHA)

		return MergeResult{PR: p, Auto: auto, Err: err}
	}
}

// MergeDone records the outcome of a merge.
func (s *State) MergeDone(res MergeResult) {
	key := res.PR.RepoShort()

	if res.Err == nil {
		slog.Info("merged", "pr", key, "auto", res.Auto)

		return
	}

	slog.Error("failed to merge", "pr", key, "auto", res.Auto, "error", res.Err)

	// GitHub refused this commit, and asking again would get the same
	// answer. Automerge tries again when the PR gets a new head commit.
	// Any other error says nothing about the commit: a base branch that
	// moved shows the PR as behind on the next fetch, and after a network
	// or server error automerge tries again once the cooldown is over.
	if res.Auto && errors.Is(res.Err, github.ErrMergeRefused) {
		s.MergeFailed[key] = true
		notify.Send(notify.AutomergeGaveUp, key+": "+res.Err.Error())
	}
}

// RerunWork re-runs the failed jobs of the PR's head commit.
func (s *State) RerunWork(p pr.PR) Work {
	client := s.Client

	return func() Result {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		err := client.RerunFailedJobs(ctx, p.Repo, p.HeadSHA)

		return RerunResult{PR: p, Err: err}
	}
}

// RerunDone records the outcome of a re-run started by the user.
func (s *State) RerunDone(res RerunResult) {
	if res.Err != nil {
		slog.Error("failed to re-run checks", "pr", res.PR.RepoShort(), "error", res.Err)

		return
	}

	slog.Info("re-running failed checks", "pr", res.PR.RepoShort())
	s.MarkRerunInFlight(res.PR.RepoShort(), res.PR.HeadSHA)
}

// autoRerunCheckWork checks run_attempt from the GitHub API, then reruns if
// appropriate.
func (s *State) autoRerunCheckWork(p pr.PR) Work {
	client := s.Client
	maxRetries := s.Config.AutoMaxRetries

	return func() Result {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		key := p.RepoShort()

		summary, err := client.FailedWorkflowRunsSummary(ctx, p.Repo, p.HeadSHA)
		if err != nil {
			return AutoRerunResult{PRKey: key, HeadSHA: p.HeadSHA, Err: err}
		}

		if summary.Count == 0 {
			return AutoRerunResult{PRKey: key, HeadSHA: p.HeadSHA, Unavailable: true}
		}

		// run_attempt=1 is the original run, so run_attempt > maxRetries means
		// we've rerun maxRetries times.
		if summary.MaxAttempt > maxRetries {
			return AutoRerunResult{PRKey: key, HeadSHA: p.HeadSHA, Failed: true}
		}

		if err := client.RerunFailedJobs(ctx, p.Repo, p.HeadSHA); err != nil {
			return AutoRerunResult{PRKey: key, HeadSHA: p.HeadSHA, Err: err}
		}

		return AutoRerunResult{PRKey: key, HeadSHA: p.HeadSHA, Reran: true, ReranAt: time.Now()}
	}
}

// AutoRerunDone records the outcome of an autorerun attempt. The error is the
// one of saving the db after a re-run was started.
func (s *State) AutoRerunDone(res AutoRerunResult) error {
	switch {
	case res.Failed:
		slog.Error("autorerun gave up", "pr", res.PRKey)
		s.RerunFailed[res.PRKey] = true
		notify.Send(notify.AutorerunGaveUp, res.PRKey+" — still failing")
	case res.Reran:
		slog.Info("autorerun triggered", "pr", res.PRKey)

		if res.ReranAt.IsZero() {
			res.ReranAt = time.Now()
		}

		s.LastAutoRerun[res.PRKey] = res.ReranAt
		s.MarkRerunInFlight(res.PRKey, res.HeadSHA)

		if err := s.SaveDB(); err != nil {
			slog.Error("failed to save db", "error", err)

			return err
		}
	case res.Unavailable:
		slog.Info("autorerun skipped, no failed GitHub Actions runs", "pr", res.PRKey)
	case res.Err != nil:
		slog.Error("autorerun error", "pr", res.PRKey, "error", res.Err)
	}

	return nil
}

// mergedCheckWork verifies whether a PR was merged, and optionally deletes the
// head branch.
func (s *State) mergedCheckWork(p pr.PR, deleteBranch bool) Work {
	client := s.Client

	return func() Result {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		key := p.RepoShort()

		state, err := client.GetPRState(ctx, p.Repo, p.Number)
		if err != nil {
			return MergedCheckResult{PRKey: key, Branch: p.HeadBranch, Err: fmt.Errorf("check merged: %w", err)}
		}

		if strings.EqualFold(state.State, "open") {
			return MergedCheckResult{PRKey: key, Branch: p.HeadBranch, Open: true}
		}

		if !state.Merged || !deleteBranch {
			return MergedCheckResult{PRKey: key, Branch: p.HeadBranch, Merged: state.Merged}
		}

		if err := client.DeleteBranch(ctx, p.HeadRepo, p.HeadBranch); err != nil {
			return MergedCheckResult{PRKey: key, Branch: p.HeadBranch, Merged: true, Err: err}
		}

		return MergedCheckResult{PRKey: key, Branch: p.HeadBranch, Merged: true, Deleted: true}
	}
}
