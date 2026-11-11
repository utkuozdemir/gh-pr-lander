// Package pr holds the domain model for a pull request and the pure logic
// that derives its actionable status. It has no knowledge of GitHub's API
// shapes (that lives in internal/github) or of the TUI. Everything here is
// deterministic and trivially unit-testable.
package pr

import (
	"fmt"
	"sort"
	"time"
)

// BaseStatus represents the PR's relationship with its base branch.
type BaseStatus int

const (
	BaseSynced   BaseStatus = iota // Up to date with base
	BaseBehind                     // Behind but rebaseable (no conflicts)
	BaseConflict                   // Has merge conflicts
	BaseUnknown                    // GitHub hasn't computed yet
)

// CIStatus represents the aggregate CI check status.
type CIStatus int

const (
	CIPassed  CIStatus = iota // All checks passed
	CIRunning                 // Some checks still running
	CIFailed                  // One or more checks failed
	CINone                    // No checks configured
)

// ReviewStatus represents the review state.
type ReviewStatus int

const (
	ReviewApproved         ReviewStatus = iota // Approved
	ReviewChangesRequested                     // Changes requested
	ReviewPending                              // Awaiting review
	ReviewNone                                 // No reviews required
)

// OverallStatus is the combined, actionable status.
type OverallStatus int

const (
	StatusReady   OverallStatus = iota // Can merge — up to date, CI pass
	StatusStale                        // Behind base, no conflicts — rebase
	StatusPending                      // CI running, otherwise fine
	StatusFailed                       // CI failed — re-run or fix
	StatusBlocked                      // Conflicts, changes requested, or blocked by GitHub
	StatusDraft                        // Draft PR
)

// CheckSummary has counts of check runs by state.
type CheckSummary struct {
	Total   int
	Passed  int
	Failed  int
	Running int
	Pending int
}

func (cs CheckSummary) String() string {
	if cs.Total == 0 {
		return "none"
	}

	if cs.Failed > 0 {
		return fmt.Sprintf("%d/%d failed", cs.Failed, cs.Total)
	}

	if cs.Running > 0 || cs.Pending > 0 {
		return fmt.Sprintf("%d/%d done", cs.Passed, cs.Total)
	}

	return fmt.Sprintf("%d/%d passed", cs.Passed, cs.Total)
}

// PR is the domain model for a pull request.
type PR struct {
	Number           int
	Title            string
	URL              string
	Repo             string // "acme/widgets"
	RepoName         string // "widgets"
	RepoOwner        string // "acme"
	HeadRepo         string // "you/widgets-fork" (fork, may differ from upstream name)
	HeadRepoOwner    string // "utkuozdemir"
	HeadRepoName     string // "widgets-fork" (fork's actual repo name)
	BaseBranch       string // "main"
	HeadBranch       string // "fix-grpc"
	HeadSHA          string
	IsDraft          bool
	BaseStatus       BaseStatus
	CIStatus         CIStatus
	ReviewStatus     ReviewStatus
	OverallStatus    OverallStatus
	Checks           CheckSummary
	MergeStateStatus string // raw from GitHub
	ReviewDecision   string // raw from GitHub
	// BehindUnenforced marks a BaseBehind PR that GitHub still reports as
	// mergeable, because its base branch does not require being up to date.
	BehindUnenforced bool
	// MergeBlocked is set when GitHub reports the merge as blocked by a branch
	// rule. Required checks that never started, such as a fork's workflows
	// waiting for a maintainer to approve them, show up only here.
	MergeBlocked bool
	// ReviewThreads is the number of inline review comment threads, of which
	// ResolvedReviewThreads are resolved.
	ReviewThreads         int
	ResolvedReviewThreads int
	// TakenOverFrom is the original author's login when the PR was opened by
	// someone else and the user committed to it. Empty for the user's own PRs.
	TakenOverFrom string
	// FailedWorkflowRuns* is populated from the Actions REST API for failed PRs.
	FailedWorkflowRunsKnown bool
	FailedWorkflowRuns      int
	MaxFailedRunAttempt     int
	LatestFailedRunUpdate   time.Time
	RunningWorkflowRuns     int
	// AwaitingApprovalRuns counts workflow runs from a fork that wait for a
	// maintainer to approve them. Only fetched for a MergeBlocked PR that
	// otherwise looks ready.
	AwaitingApprovalRuns int
}

// RepoShort is the stable key used to identify a PR everywhere, e.g.
// "acme/widgets#123".
func (p PR) RepoShort() string {
	return fmt.Sprintf("%s#%d", p.Repo, p.Number)
}

// ComputeOverallStatus derives the single actionable status from the
// individual base/CI/review signals.
func ComputeOverallStatus(p PR) OverallStatus {
	if p.IsDraft {
		return StatusDraft
	}

	if p.BaseStatus == BaseConflict || p.ReviewStatus == ReviewChangesRequested {
		return StatusBlocked
	}

	if p.CIStatus == CIFailed {
		return StatusFailed
	}

	if p.BaseStatus == BaseBehind {
		return StatusStale
	}

	if p.CIStatus == CIRunning {
		return StatusPending
	}

	if p.ReviewStatus == ReviewPending {
		return StatusPending
	}

	if p.BaseStatus == BaseSynced && (p.CIStatus == CIPassed || p.CIStatus == CINone) {
		if p.MergeBlocked {
			return StatusBlocked
		}

		return StatusReady
	}

	return StatusPending
}

// Sort orders PRs by actionable status, then repo, then number, in place.
func Sort(prs []PR) {
	sort.SliceStable(prs, func(i, j int) bool {
		if prs[i].OverallStatus != prs[j].OverallStatus {
			return prs[i].OverallStatus < prs[j].OverallStatus
		}

		if prs[i].Repo != prs[j].Repo {
			return prs[i].Repo < prs[j].Repo
		}

		return prs[i].Number < prs[j].Number
	})
}
