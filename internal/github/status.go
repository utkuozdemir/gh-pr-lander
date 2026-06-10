package github

import (
	"strings"

	"github.com/utkuozdemir/sidero-rebaser/internal/pr"
)

// prFromNode maps a GraphQL PR node into the domain model, deriving all status
// fields. This is the single boundary where GitHub's API shapes turn into our
// own model — nothing downstream should ever see a prNode.
func prFromNode(node prNode) pr.PR {
	p := pr.PR{
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
		p.HeadRepo = node.HeadRepository.NameWithOwner
		p.HeadRepoOwner = node.HeadRepository.Owner.Login
		p.HeadRepoName = node.HeadRepository.Name
	}

	if node.HeadRef.Target.OID != "" {
		p.HeadSHA = node.HeadRef.Target.OID
	}

	if p.HeadSHA == "" && len(node.Commits.Nodes) > 0 {
		p.HeadSHA = node.Commits.Nodes[0].Commit.OID
	}

	p.BaseStatus = computeBaseStatus(node)
	p.CIStatus, p.Checks = computeCIStatus(node)
	p.ReviewStatus = computeReviewStatus(node)
	p.OverallStatus = pr.ComputeOverallStatus(p)

	return p
}

func computeBaseStatus(node prNode) pr.BaseStatus {
	switch strings.ToUpper(node.MergeStateStatus) {
	case "CLEAN":
		return pr.BaseSynced
	case "BEHIND":
		return pr.BaseBehind
	case "DIRTY":
		return pr.BaseConflict
	case "UNSTABLE":
		return pr.BaseSynced // Synced but checks failing
	case "BLOCKED":
		// Could be behind or synced — check mergeable field
		switch strings.ToUpper(node.Mergeable) {
		case "CONFLICTING":
			return pr.BaseConflict
		case "MERGEABLE":
			return pr.BaseSynced
		default:
			return pr.BaseUnknown
		}
	case "HAS_HOOKS":
		return pr.BaseSynced
	default:
		switch strings.ToUpper(node.Mergeable) {
		case "CONFLICTING":
			return pr.BaseConflict
		case "MERGEABLE":
			return pr.BaseSynced
		default:
			return pr.BaseUnknown
		}
	}
}

func computeCIStatus(node prNode) (pr.CIStatus, pr.CheckSummary) {
	summary := pr.CheckSummary{}

	if len(node.Commits.Nodes) == 0 {
		return pr.CINone, summary
	}

	commit := node.Commits.Nodes[0].Commit
	if commit.StatusCheckRollup == nil {
		return pr.CINone, summary
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

	// If the total count from GH is higher, there are more checks we didn't
	// fetch. Treat unfetched checks as pending (they could be failing — safer
	// than assuming passed).
	if unfetched := commit.StatusCheckRollup.Contexts.TotalCount - summary.Total; unfetched > 0 {
		summary.Pending += unfetched
		summary.Total += unfetched
	}

	if summary.Failed > 0 {
		return pr.CIFailed, summary
	}

	if summary.Running > 0 || summary.Pending > 0 {
		return pr.CIRunning, summary
	}

	if summary.Passed > 0 {
		return pr.CIPassed, summary
	}

	return pr.CINone, summary
}

func computeReviewStatus(node prNode) pr.ReviewStatus {
	switch strings.ToUpper(node.ReviewDecision) {
	case "APPROVED":
		return pr.ReviewApproved
	case "CHANGES_REQUESTED":
		return pr.ReviewChangesRequested
	case "REVIEW_REQUIRED":
		return pr.ReviewPending
	default:
		return pr.ReviewNone
	}
}

func isFailedWorkflowConclusion(conclusion string) bool {
	switch conclusion {
	case "failure", "timed_out", "cancelled":
		return true
	default:
		return false
	}
}
