package github

import (
	"strings"

	"github.com/utkuozdemir/gh-pr-lander/internal/pr"
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

	// Threads beyond the first 100 are not fetched and count as unresolved.
	p.ReviewThreads = node.ReviewThreads.TotalCount
	for _, thread := range node.ReviewThreads.Nodes {
		if thread.IsResolved {
			p.ResolvedReviewThreads++
		}
	}

	p.BaseStatus = computeBaseStatus(node)
	if p.BaseStatus == pr.BaseSynced && behindBase(node, p.HeadSHA) {
		p.BaseStatus = pr.BaseBehind
		// UNKNOWN means GitHub has not computed the merge state yet, so it says
		// nothing about whether the branch enforces being up to date.
		p.BehindUnenforced = !strings.EqualFold(node.MergeStateStatus, "UNKNOWN")
	}

	p.MergeBlocked = strings.EqualFold(node.MergeStateStatus, "BLOCKED")
	p.CIStatus, p.Checks = computeCIStatus(node)
	p.ReviewStatus = computeReviewStatus(node)
	p.OverallStatus = pr.ComputeOverallStatus(p)

	return p
}

// touchedBy reports whether the user opened the PR or authored or committed any
// of its last 100 commits, as happens when taking over a community PR to rebase
// and re-sign it.
func touchedBy(node prNode, username string) bool {
	if strings.EqualFold(node.Author.Login, username) {
		return true
	}

	for _, c := range node.AllCommits.Nodes {
		for _, actor := range []gitActor{c.Commit.Author, c.Commit.Committer} {
			if actor.User != nil && strings.EqualFold(actor.User.Login, username) {
				return true
			}
		}
	}

	return false
}

// behindBase reports whether the base branch has moved past the commit the PR
// is based on. GitHub only reports BEHIND when the base branch requires
// branches to be up to date and has required checks, so without this a PR in
// a repo that does not enforce it shows as synced however far behind it is.
//
// A base branch that sits on the PR's own head commit has not moved past the
// PR: the PR was just fast-forward merged, and GitHub lists it as open for a
// moment longer.
func behindBase(node prNode, headSHA string) bool {
	if node.BaseRefOID == "" || node.BaseRef == nil || node.BaseRef.Target.OID == "" {
		return false
	}

	tip := node.BaseRef.Target.OID

	return node.BaseRefOID != tip && headSHA != tip
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
		// GitHub computes UNSTABLE for a PR that could be merged right now
		// while a check that is not required has not passed. So every required
		// check is green, and whatever is still open does not hold up a merge.
		// A check that never reports, such as a coverage service waiting for
		// an upload, would otherwise keep a mergeable PR waiting for good.
		if strings.EqualFold(node.MergeStateStatus, "UNSTABLE") {
			summary.Optional = summary.Running + summary.Pending

			return pr.CIPassed, summary
		}

		return pr.CIRunning, summary
	}

	// GitHub's rollup can still report that checks are expected or pending in
	// aggregate even when every context we fetched looks passed — typically
	// right after a push, before all required checks have registered on the new
	// commit. Trust that aggregate signal over an incomplete context list so we
	// do not flip to "passed" during the gap.
	switch strings.ToUpper(commit.StatusCheckRollup.State) {
	case "EXPECTED", "PENDING":
		if summary.Total > 0 {
			return pr.CIRunning, summary
		}
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
