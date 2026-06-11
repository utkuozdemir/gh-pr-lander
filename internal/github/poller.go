package github

import (
	"context"
	"time"

	"github.com/utkuozdemir/sidero-rebaser/internal/pr"
)

// fetchTimeout bounds a single poll cycle's API work.
const fetchTimeout = 30 * time.Second

// PRFetcher is the slice of the GitHub client the poller depends on. Declaring
// it here (and taking it as an interface) lets the poller be unit-tested with a
// fake fetcher, no network required.
type PRFetcher interface {
	FetchPRs(ctx context.Context, orgs []string) ([]pr.PR, error)
}

// FetchResult is one poll outcome streamed to the UI.
type FetchResult struct {
	PRs []pr.PR
	Err error
}

// Poller continuously fetches PRs in the background and streams results over a
// channel. It replaces a UI-driven "tick then fetch" loop: the UI just reads
// results as they arrive, and can ask for an immediate refresh after an action
// (post /m, rerun, rebase) so effects show up without waiting for the next
// interval. This is what makes the tool feel responsive (issue #2).
type Poller struct {
	fetcher  PRFetcher
	orgs     []string
	interval time.Duration
	results  chan FetchResult
	refresh  chan struct{}
}

// NewPoller builds a poller. The caller owns the lifecycle via Run(ctx).
func NewPoller(fetcher PRFetcher, orgs []string, interval time.Duration) *Poller {
	return &Poller{
		fetcher:  fetcher,
		orgs:     orgs,
		interval: interval,
		results:  make(chan FetchResult, 1),
		refresh:  make(chan struct{}, 1),
	}
}

// Results is the read-only stream of poll outcomes.
func (p *Poller) Results() <-chan FetchResult {
	return p.results
}

// Refresh asks the poller to fetch immediately. It never blocks: if a refresh
// is already pending, the request is coalesced.
func (p *Poller) Refresh() {
	select {
	case p.refresh <- struct{}{}:
	default:
	}
}

// Run fetches once immediately, then on every interval tick and on every
// refresh request, until ctx is cancelled. It is meant to be driven by a
// structured-concurrency group (errgroup) and returns ctx.Err() so the group
// can observe cancellation.
func (p *Poller) Run(ctx context.Context) error {
	timer := time.NewTimer(p.interval)
	defer timer.Stop()

	for {
		p.fetchOnce(ctx)

		// Reset the interval timer after each fetch so the gap is measured
		// from completion, not from the previous tick.
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}

		timer.Reset(p.interval)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		case <-p.refresh:
		}
	}
}

func (p *Poller) fetchOnce(ctx context.Context) {
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	prs, err := p.fetcher.FetchPRs(fctx, p.orgs)

	cancel()

	select {
	case p.results <- FetchResult{PRs: prs, Err: err}:
	case <-ctx.Done():
	}
}
