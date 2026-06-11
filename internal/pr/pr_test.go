package pr

import "testing"

func TestComputeOverallStatusKeepsPendingReviewOutOfReady(t *testing.T) {
	status := ComputeOverallStatus(PR{
		BaseStatus:   BaseSynced,
		CIStatus:     CIPassed,
		ReviewStatus: ReviewPending,
	})

	if status != StatusPending {
		t.Fatalf("expected StatusPending, got %v", status)
	}
}

func TestComputeOverallStatusReadyWhenSyncedAndPassing(t *testing.T) {
	status := ComputeOverallStatus(PR{
		BaseStatus:   BaseSynced,
		CIStatus:     CIPassed,
		ReviewStatus: ReviewApproved,
	})

	if status != StatusReady {
		t.Fatalf("expected StatusReady, got %v", status)
	}
}

func TestComputeOverallStatusFailedCITrumpsStale(t *testing.T) {
	status := ComputeOverallStatus(PR{
		BaseStatus: BaseBehind,
		CIStatus:   CIFailed,
	})

	if status != StatusFailed {
		t.Fatalf("expected StatusFailed, got %v", status)
	}
}
