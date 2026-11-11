package tui

import (
	"context"

	"github.com/utkuozdemir/sidero-rebaser/internal/github"
)

// Client is the slice of the GitHub API the TUI depends on. It is declared here
// (the consumer side) so the TUI can be unit-tested with a fake implementation
// and never reaches the network. *github.Client satisfies it.
type Client interface {
	FastForwardMerge(ctx context.Context, repo, baseBranch, headSHA string) error
	FailedWorkflowRunsSummary(ctx context.Context, repo, headSHA string) (github.WorkflowSummary, error)
	RerunFailedJobs(ctx context.Context, repo, headSHA string) error
	ApproveWorkflowRuns(ctx context.Context, repo, headSHA string) error
	GetPRState(ctx context.Context, repo string, prNumber int) (github.PRState, error)
	DeleteBranch(ctx context.Context, repo, branch string) error
	Username() string
}
