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
