package main

import "testing"

func TestComputeCIStatusTreatsUnfetchedChecksAsPending(t *testing.T) {
	status, summary := computeCIStatus(prNode{
		Commits: commitConn{Nodes: []commitNode{{
			Commit: commitDetail{StatusCheckRollup: &statusCheckRollup{
				Contexts: contextConn{
					TotalCount: 2,
					Nodes: []contextNode{{
						TypeName:   "CheckRun",
						Status:     "COMPLETED",
						Conclusion: "SUCCESS",
					}},
				},
			}},
		}}},
	})

	if status != CIRunning {
		t.Fatalf("expected CIRunning for unfetched checks, got %v", status)
	}
	if summary.Total != 2 || summary.Pending != 1 || summary.Passed != 1 {
		t.Fatalf("unexpected summary: %#v", summary)
	}
}

func TestComputeOverallStatusKeepsPendingReviewOutOfReady(t *testing.T) {
	status := computeOverallStatus(PR{
		BaseStatus:   BaseSynced,
		CIStatus:     CIPassed,
		ReviewStatus: ReviewPending,
	})

	if status != StatusPending {
		t.Fatalf("expected StatusPending, got %v", status)
	}
}
