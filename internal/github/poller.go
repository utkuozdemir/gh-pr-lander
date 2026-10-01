package github

import (
	"context"
	"time"

	"github.com/utkuozdemir/sidero-rebaser/internal/pr"
)

const (
	// fetchTimeout bounds a single poll cycle's API work.
	fetchTimeout = 30 * time.Second

	// minActiveInterval is the floor for the fast cadence used while a watched
	// PR is in a transitional state (CI running, mergeability not yet computed).
	// It keeps us from hammering the API / burning rate limit while still
	// reacting to a CI pass or failure within seconds rather than minutes.
	minActiveInterval = 15 * time.Second
)

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
// refresh request, until ctx is cancelled. The interval is adaptive: while a
// watched PR is in a transitional state (CI running, mergeability unknown) the
// next tick is shortened so a CI pass or failure is picked up within seconds,
// not minutes; when everything is idle it relaxes back to the configured
// interval. GitHub has no push/websocket API a local tool could subscribe to,
// so polling is the only option (see AGENTS.md), and this keeps it both
// responsive and cheap. It is meant to be driven by a structured-concurrency
// group (errgroup) and returns ctx.Err() so the group can observe cancellation.
func (p *Poller) Run(ctx context.Context) error {
	timer := time.NewTimer(p.interval)
	defer timer.Stop()

	for {
		prs := p.fetchOnce(ctx)

		// Reset the interval timer after each fetch so the gap is measured
		// from completion, not from the previous tick.
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}

		timer.Reset(p.nextInterval(prs))

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
		case <-p.refresh:
		}
	}
}

func (p *Poller) fetchOnce(ctx context.Context) []pr.PR {
	fctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	prs, err := p.fetcher.FetchPRs(fctx, p.orgs)

	cancel()

	select {
	case p.results <- FetchResult{PRs: prs, Err: err}:
	case <-ctx.Done():
	}

	return prs
}

// nextInterval picks the delay before the next fetch: the fast cadence when any
// PR is transitional, otherwise the configured interval. On a fetch error prs
// is nil and we fall back to the configured interval, so an outage backs off to
// the normal cadence rather than fast-spinning.
func (p *Poller) nextInterval(prs []pr.PR) time.Duration {
	if !anyTransitional(prs) {
		return p.interval
	}

	// Quarter of the configured interval, floored so we don't hammer the API
	// and capped so the fast cadence is never slower than the normal one.
	return min(max(p.interval/4, minActiveInterval), p.interval)
}

// anyTransitional reports whether any PR is in a state that is expected to flip
// soon, making a faster poll worthwhile. CI running is the key case (we want to
// catch the pass/fail the moment it lands); base unknown means GitHub is still
// computing mergeability and will resolve shortly.
func anyTransitional(prs []pr.PR) bool {
	for _, p := range prs {
		if p.CIStatus == pr.CIRunning || p.BaseStatus == pr.BaseUnknown {
			return true
		}
	}

	return false
}
