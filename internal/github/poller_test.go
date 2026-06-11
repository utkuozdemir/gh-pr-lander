package github

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/utkuozdemir/sidero-rebaser/internal/pr"
)

type fakeFetcher struct {
	calls atomic.Int32
}

func (f *fakeFetcher) FetchPRs(_ context.Context, _ []string) ([]pr.PR, error) {
	n := f.calls.Add(1)

	return []pr.PR{{Number: int(n), Repo: "org/repo"}}, nil
}

func TestPollerFetchesImmediatelyThenOnRefresh(t *testing.T) {
	fetcher := &fakeFetcher{}
	// Long interval so only the immediate fetch and the refresh fire.
	p := NewPoller(fetcher, []string{"org"}, time.Hour)

	ctx := t.Context()

	go func() { _ = p.Run(ctx) }()

	// First (immediate) fetch.
	select {
	case res := <-p.Results():
		if res.Err != nil {
			t.Fatalf("unexpected error: %v", res.Err)
		}

		if res.PRs[0].Number != 1 {
			t.Fatalf("expected first fetch, got PR %d", res.PRs[0].Number)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for immediate fetch")
	}

	// Ask for an immediate refresh and expect a second fetch quickly.
	p.Refresh()

	select {
	case res := <-p.Results():
		if res.PRs[0].Number != 2 {
			t.Fatalf("expected second fetch, got PR %d", res.PRs[0].Number)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for refresh fetch")
	}
}

func TestNextIntervalIsAdaptive(t *testing.T) {
	p := NewPoller(&fakeFetcher{}, []string{"org"}, 2*time.Minute)

	idle := []pr.PR{{CIStatus: pr.CIPassed, BaseStatus: pr.BaseSynced}}
	if got := p.nextInterval(idle); got != 2*time.Minute {
		t.Fatalf("idle PRs should poll at the configured interval, got %s", got)
	}

	// nil (e.g. after a fetch error) also relaxes to the configured interval.
	if got := p.nextInterval(nil); got != 2*time.Minute {
		t.Fatalf("nil PRs should poll at the configured interval, got %s", got)
	}

	for _, active := range []pr.PR{
		{CIStatus: pr.CIRunning, BaseStatus: pr.BaseSynced},
		{CIStatus: pr.CIPassed, BaseStatus: pr.BaseUnknown},
	} {
		got := p.nextInterval([]pr.PR{active})
		if got != 30*time.Second { // quarter of 2m, above the 15s floor
			t.Fatalf("transitional PR should poll fast, got %s", got)
		}
	}
}

func TestNextIntervalRespectsFloorAndCap(t *testing.T) {
	// A short configured interval: the fast cadence is floored at
	// minActiveInterval but never exceeds the configured interval.
	p := NewPoller(&fakeFetcher{}, []string{"org"}, 10*time.Second)

	active := []pr.PR{{CIStatus: pr.CIRunning}}
	if got := p.nextInterval(active); got != 10*time.Second {
		t.Fatalf("fast cadence must not exceed the configured interval, got %s", got)
	}
}

func TestPollerStopsOnContextCancel(t *testing.T) {
	fetcher := &fakeFetcher{}
	p := NewPoller(fetcher, []string{"org"}, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- p.Run(ctx) }()

	// Drain the immediate fetch.
	<-p.Results()

	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected context error on cancel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("poller did not stop on cancel")
	}
}
