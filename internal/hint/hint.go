// Package hint holds the one-line explanations of what each control and each
// status means. Both front ends show them, the page as hover text and the
// terminal app in its help view, so they are written once here.
package hint

import "fmt"

// Controls.
const (
	Automerge        = "Merge this pull request as soon as it is ready."
	Approvals        = "The number of approvals automerge waits for."
	ApprovalsDefault = "Automerge waits for the approvals the repository requires."
	Comments         = "Whether automerge waits for review comments to be resolved."
	CommentsAny      = "Automerge does not look at review comments."
	CommentsResolved = "Automerge waits until every review comment is resolved."
	Autorerun        = "Re-run failed GitHub Actions jobs without asking. It gives up after a few tries on the same commit."
	Merge            = "Fast-forward the base branch to this pull request. No merge commit is created."
	MergeNotReady    = "This pull request is not ready to merge yet."
	Rerun            = "Re-run the failed GitHub Actions jobs of the latest commit."
	RerunNothing     = "There are no failed checks to re-run."
	Refresh          = "Fetch the pull requests from GitHub now."
	Open             = "Open the pull request on GitHub."
)

// ApprovalsAtLeast explains an approvals condition of two or more.
func ApprovalsAtLeast(approvals int) string {
	return fmt.Sprintf("Automerge waits for %d approvals.", approvals)
}

// What a pull request's overall status means.
const (
	Ready       = "Up to date, every required check passed, and it has the approvals the repository requires. It can be merged."
	Stale       = "The base branch has new commits. It needs a rebase before it can be merged."
	Pending     = "Waiting for checks to finish or for a review."
	Failed      = "A check failed."
	Blocked     = "It has conflicts, changes were requested, or a branch rule blocks the merge."
	Draft       = "A draft. It is not merged until it is marked as ready for review."
	MergeGaveUp = "GitHub refused to merge this commit. Automerge tries again when there is a new commit."
	RerunGaveUp = "The re-runs kept failing. Autorerun tries again when there is a new commit."
	Merged      = "It was merged."
	Closed      = "It was closed without being merged."
	Checking    = "It left the list. Checking whether it was merged."
)

// What the parts of a pull request's status line mean.
const (
	BaseSynced       = "Up to date with the base branch."
	BaseBehind       = "The base branch has new commits. It needs a rebase."
	BaseUnenforced   = "The base branch has new commits. The repository does not require being up to date, but a fast-forward merge does."
	BaseConflict     = "It conflicts with the base branch."
	BaseUnknown      = "GitHub is still working this out."
	Checks           = "The checks on the latest commit."
	ChecksOptional   = "Every required check passed. The ones still running are optional and do not hold back a merge."
	ChecksUnapproved = "A workflow waits for a maintainer to approve it before it runs."
	Review           = "The review state. The number in parentheses is how many people approved."
	CommentCount     = "Resolved review comments, out of all of them."
	Reruns           = "How many times the checks of the latest commit were re-run, by anyone. With autorerun on, it gives up at the limit."
	Note             = "A comment of yours that nobody replied to. It does not hold back automerge."
	TakenOver        = "Opened by someone else. You committed to it, so it is listed here."
	QueueActive      = "This pull request holds its autorebase queue. The ones behind it wait for it to merge."
	QueueWaiting     = "Its place in the autorebase queue. It is rebased when the ones before it are merged."
	Updated          = "When the list was last fetched from GitHub."
	Version          = "The version that is running."
)
