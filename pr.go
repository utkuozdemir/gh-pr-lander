package main

import (
	"fmt"
	"sort"
	"strings"
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
	StatusBlocked                      // Conflicts or changes requested
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
	Repo             string // "siderolabs/talos"
	RepoName         string // "talos"
	RepoOwner        string // "siderolabs"
	HeadRepo         string // "utkuozdemir/cosi-runtime" (fork, may differ from upstream name)
	HeadRepoOwner    string // "utkuozdemir"
	HeadRepoName     string // "cosi-runtime" (fork's actual repo name)
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
	// FailedWorkflowRuns* is populated from the Actions REST API for failed PRs.
	FailedWorkflowRunsKnown bool
	FailedWorkflowRuns      int
	MaxFailedRunAttempt     int
	LatestFailedRunUpdate   time.Time
}

func (pr PR) RepoShort() string {
	return fmt.Sprintf("%s#%d", pr.Repo, pr.Number)
}

func prFromNode(node prNode) PR {
	pr := PR{
		Number:           node.Number,
		Title:            node.Title,
		URL:              node.URL,
		Repo:             node.Repository.NameWithOwner,
		RepoName:         node.Repository.Name,
		RepoOwner:        node.Repository.Owner.Login,
		BaseBranch:       node.BaseRefName,
		HeadBranch:       node.HeadRefName,
		IsDraft:          node.IsDraft,
		MergeStateStatus: node.MergeStateStatus,
		ReviewDecision:   node.ReviewDecision,
	}

	if node.HeadRepository != nil {
		pr.HeadRepo = node.HeadRepository.NameWithOwner
		pr.HeadRepoOwner = node.HeadRepository.Owner.Login
		pr.HeadRepoName = node.HeadRepository.Name
	}

	if node.HeadRef.Target.OID != "" {
		pr.HeadSHA = node.HeadRef.Target.OID
	}

	// Extract head SHA from commits if not in headRef
	if pr.HeadSHA == "" && len(node.Commits.Nodes) > 0 {
		pr.HeadSHA = node.Commits.Nodes[0].Commit.OID
	}

	pr.BaseStatus = computeBaseStatus(node)
	pr.CIStatus, pr.Checks = computeCIStatus(node)
	pr.ReviewStatus = computeReviewStatus(node)
	pr.OverallStatus = computeOverallStatus(pr)

	return pr
}

func computeBaseStatus(node prNode) BaseStatus {
	switch strings.ToUpper(node.MergeStateStatus) {
	case "CLEAN":
		return BaseSynced
	case "BEHIND":
		return BaseBehind
	case "DIRTY":
		return BaseConflict
	case "UNSTABLE":
		return BaseSynced // Synced but checks failing
	case "BLOCKED":
		// Could be behind or synced — check mergeable field
		switch strings.ToUpper(node.Mergeable) {
		case "CONFLICTING":
			return BaseConflict
		case "MERGEABLE":
			return BaseSynced
		default:
			return BaseUnknown
		}
	case "HAS_HOOKS":
		return BaseSynced
	default:
		switch strings.ToUpper(node.Mergeable) {
		case "CONFLICTING":
			return BaseConflict
		case "MERGEABLE":
			return BaseSynced
		default:
			return BaseUnknown
		}
	}
}

func computeCIStatus(node prNode) (CIStatus, CheckSummary) {
	summary := CheckSummary{}

	if len(node.Commits.Nodes) == 0 {
		return CINone, summary
	}

	commit := node.Commits.Nodes[0].Commit
	if commit.StatusCheckRollup == nil {
		return CINone, summary
	}

	for _, ctx := range commit.StatusCheckRollup.Contexts.Nodes {
		summary.Total++
		switch ctx.TypeName {
		case "CheckRun":
			switch strings.ToUpper(ctx.Status) {
			case "COMPLETED":
				switch strings.ToUpper(ctx.Conclusion) {
				case "SUCCESS", "NEUTRAL", "SKIPPED":
					summary.Passed++
				default:
					summary.Failed++
				}
			case "IN_PROGRESS":
				summary.Running++
			default:
				summary.Pending++
			}
		case "StatusContext":
			switch strings.ToUpper(ctx.State) {
			case "SUCCESS":
				summary.Passed++
			case "FAILURE", "ERROR":
				summary.Failed++
			case "PENDING", "EXPECTED":
				summary.Pending++
			default:
				summary.Pending++
			}
		}
	}

	// If the total count from GH is higher, there are more checks we didn't fetch.
	// Treat unfetched checks as pending (they could be failing — safer than assuming passed).
	if unfetched := commit.StatusCheckRollup.Contexts.TotalCount - summary.Total; unfetched > 0 {
		summary.Pending += unfetched
		summary.Total += unfetched
	}

	if summary.Failed > 0 {
		return CIFailed, summary
	}
	if summary.Running > 0 || summary.Pending > 0 {
		return CIRunning, summary
	}
	if summary.Passed > 0 {
		return CIPassed, summary
	}
	return CINone, summary
}

func computeReviewStatus(node prNode) ReviewStatus {
	switch strings.ToUpper(node.ReviewDecision) {
	case "APPROVED":
		return ReviewApproved
	case "CHANGES_REQUESTED":
		return ReviewChangesRequested
	case "REVIEW_REQUIRED":
		return ReviewPending
	default:
		return ReviewNone
	}
}

func computeOverallStatus(pr PR) OverallStatus {
	if pr.IsDraft {
		return StatusDraft
	}
	if pr.BaseStatus == BaseConflict || pr.ReviewStatus == ReviewChangesRequested {
		return StatusBlocked
	}
	if pr.CIStatus == CIFailed {
		return StatusFailed
	}
	if pr.BaseStatus == BaseBehind {
		return StatusStale
	}
	if pr.CIStatus == CIRunning {
		return StatusPending
	}
	if pr.ReviewStatus == ReviewPending {
		return StatusPending
	}
	if pr.BaseStatus == BaseSynced && (pr.CIStatus == CIPassed || pr.CIStatus == CINone) {
		return StatusReady
	}
	return StatusPending
}

func sortPRs(prs []PR) {
	sort.SliceStable(prs, func(i, j int) bool {
		if prs[i].OverallStatus != prs[j].OverallStatus {
			return prs[i].OverallStatus < prs[j].OverallStatus
		}
		// Within same status, sort by repo then number
		if prs[i].Repo != prs[j].Repo {
			return prs[i].Repo < prs[j].Repo
		}
		return prs[i].Number < prs[j].Number
	})
}
