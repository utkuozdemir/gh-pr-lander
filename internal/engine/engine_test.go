package engine

import (
	"context"
	"testing"
	"time"

	"github.com/utkuozdemir/gh-pr-lander/internal/config"
	"github.com/utkuozdemir/gh-pr-lander/internal/github"
	"github.com/utkuozdemir/gh-pr-lander/internal/pr"
)

// fakeClient is a no-op Client used to exercise the state without any network.
type fakeClient struct {
	mergeErr error
}

func (f fakeClient) FastForwardMerge(context.Context, string, string, string) error {
	return f.mergeErr
}

func (f fakeClient) FailedWorkflowRunsSummary(context.Context, string, string) (github.WorkflowSummary, error) {
	return github.WorkflowSummary{}, nil
}
func (f fakeClient) RerunFailedJobs(context.Context, string, string) error     { return nil }
func (f fakeClient) ApproveWorkflowRuns(context.Context, string, string) error { return nil }
func (f fakeClient) GetPRState(context.Context, string, int) (github.PRState, error) {
	return github.PRState{}, nil
}
func (f fakeClient) DeleteBranch(context.Context, string, string) error { return nil }
func (f fakeClient) Username() string                                   { return "" }

// testState is a state with one PR and every switch off.
func testState(t *testing.T, p pr.PR) *State {
	t.Helper()

	return &State{
		Client: fakeClient{},
		Config: config.Config{
			DataDir:        t.TempDir(),
			PollInterval:   2 * time.Minute,
			AutoCooldown:   time.Hour,
			AutoMaxRetries: 3,
		},
		PRs:              []pr.PR{p},
		AutoMerge:        map[string]bool{},
		MinApprovals:     map[string]int{},
		ResolvedOnly:     map[string]bool{},
		AutoRerun:        map[string]bool{},
		LastAutoRerun:    map[string]time.Time{},
		LastRerun:        map[string]time.Time{},
		MergeFailed:      map[string]bool{},
		RerunFailed:      map[string]bool{},
		LastAutoCheck:    map[string]time.Time{},
		RerunInFlight:    map[string]time.Time{},
		RerunInFlightSHA: map[string]string{},
	}
}
