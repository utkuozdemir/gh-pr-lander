// Package engine holds everything the tool does with pull requests that needs
// no user interface and no signing: the automerge and autorerun switches, the
// guards against GitHub's transient check readings, merged detection, and the
// GitHub calls behind merge and re-run.
//
// State is plain data with synchronous methods. It starts no goroutines. GitHub
// calls are handed back as Work for the caller to run wherever it likes (a
// Bubble Tea command in the terminal app, a goroutine in the server), and the
// Result goes back into the matching Done method. A caller that uses State from
// several goroutines guards it with its own lock.
package engine

import (
	"context"
	"sort"
	"time"

	"github.com/utkuozdemir/sidero-rebaser/internal/config"
	"github.com/utkuozdemir/sidero-rebaser/internal/github"
	"github.com/utkuozdemir/sidero-rebaser/internal/pr"
	"github.com/utkuozdemir/sidero-rebaser/internal/store"
)

// Client is the slice of the GitHub API the engine and its front ends depend
// on. It is declared here (the consumer side) so they can be unit-tested with a
// fake implementation and never reach the network. *github.Client satisfies it.
type Client interface {
	FastForwardMerge(ctx context.Context, repo, baseBranch, headSHA string) error
	FailedWorkflowRunsSummary(ctx context.Context, repo, headSHA string) (github.WorkflowSummary, error)
	RerunFailedJobs(ctx context.Context, repo, headSHA string) error
	ApproveWorkflowRuns(ctx context.Context, repo, headSHA string) error
	GetPRState(ctx context.Context, repo string, prNumber int) (github.PRState, error)
	DeleteBranch(ctx context.Context, repo, branch string) error
	Username() string
}

// MergedPR tracks a PR that disappeared from the open-PR search.
type MergedPR struct {
	PR        pr.PR
	GoneAt    time.Time
	Confirmed bool // API check completed
	Merged    bool // true = merged, false = closed without merge
}

// State is what the tool knows about the watched PRs and what it was asked to
// do with them.
type State struct {
	Config config.Config
	Client Client

	PRs     []pr.PR
	PrevPRs []pr.PR

	LastRerun      map[string]time.Time // manual rerun cooldown
	AutoMerge      map[string]bool      // enabled flag, persisted in db.yaml
	AutoRerun      map[string]bool      // enabled flag, persisted in db.yaml
	LastAutoRerun  map[string]time.Time // persisted auto-rerun cooldown
	AutoRebase     map[string]int       // queue order, persisted in db.yaml, acted on by the terminal app only
	MergeFailed    map[string]bool      // discovered from API, in-memory
	RerunFailed    map[string]bool      // discovered from API, in-memory
	LastAutoCheck  map[string]time.Time // cooldown gate to avoid API spam
	RecentlyMerged map[string]*MergedPR // PRs that disappeared (merged/closed)

	// RerunInFlight guards against GitHub momentarily reporting "all checks
	// passed" right after a re-run is triggered (issue #1). While a PR is in
	// flight, a transient passed state is treated as still-running so automerge
	// does not fire a spurious merge. Keyed by PR; value is the trigger time and
	// the head SHA at that moment.
	RerunInFlight    map[string]time.Time
	RerunInFlightSHA map[string]string

	// CheckBaseline guards against GitHub momentarily reporting an incomplete,
	// all-green check set right after an external rebase+push, before the new
	// commit's required checks have registered (issue #7). Keyed by PR.
	CheckBaseline map[string]CheckBaseline
}

// CheckBaseline remembers, per PR, the head SHA we last saw, when we first saw
// it, the high-water check count observed on it, and the high-water count on the
// SHA before it. A new commit whose current check count is below the previous
// commit's count is still settling.
type CheckBaseline struct {
	SHA          string
	FirstSeen    time.Time
	MaxTotal     int
	PrevMaxTotal int
}

// New builds the state from the db file in the config's data directory. On a
// load error the state is still usable, with every switch off.
func New(cfg config.Config, client Client) (*State, error) {
	db, err := store.Load(cfg.DataDir)

	return &State{
		Config:           cfg,
		Client:           client,
		LastRerun:        make(map[string]time.Time),
		AutoMerge:        db.AutoMerge,
		AutoRerun:        db.AutoRerun,
		LastAutoRerun:    db.AutoRerunLast,
		AutoRebase:       db.AutoRebase,
		MergeFailed:      make(map[string]bool),
		RerunFailed:      make(map[string]bool),
		LastAutoCheck:    make(map[string]time.Time),
		RecentlyMerged:   make(map[string]*MergedPR),
		RerunInFlight:    make(map[string]time.Time),
		RerunInFlightSHA: make(map[string]string),
		CheckBaseline:    make(map[string]CheckBaseline),
	}, err
}

// SaveDB writes the switches to the db file.
func (s *State) SaveDB() error {
	return store.Save(s.Config.DataDir, store.DB{
		AutoMerge:     s.AutoMerge,
		AutoRerun:     s.AutoRerun,
		AutoRerunLast: s.LastAutoRerun,
		AutoRebase:    s.AutoRebase,
	})
}

// Apply takes one successful fetch: it applies the settling guards, notifies
// about status changes, and notices PRs that left the list. It returns the
// checks to run for those PRs, and the error of saving the db if switches of
// gone PRs were dropped.
func (s *State) Apply(prs []pr.PR) ([]Work, error) {
	s.PrevPRs = s.PRs
	s.PRs = s.applyChecksSettling(s.applyRerunSettling(prs))

	s.checkTransitions()
	works := s.detectMerged()

	return works, s.cleanupAuto()
}

// SortedMerged returns the PRs that left the list recently, newest first.
func (s *State) SortedMerged() []*MergedPR {
	now := time.Now()

	var result []*MergedPR

	for _, mp := range s.RecentlyMerged {
		if now.Sub(mp.GoneAt) < s.Config.MergedDisplayDuration {
			result = append(result, mp)
		}
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].GoneAt.After(result[j].GoneAt)
	})

	return result
}
