// Package server is the front end with no terminal: an always-on process that
// runs the engine's automation and serves a small page to flip its switches
// from a phone or a browser. It does everything that needs no signing. Rebase
// stays with the terminal app.
package server

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/utkuozdemir/gh-pr-lander/internal/config"
	"github.com/utkuozdemir/gh-pr-lander/internal/engine"
	"github.com/utkuozdemir/gh-pr-lander/internal/github"
	"github.com/utkuozdemir/gh-pr-lander/internal/pr"
)

//go:embed page.html static/htmx.min.js static/favicon.svg static/favicon-32.png static/apple-touch-icon.png
var assets embed.FS

// Server owns the engine state. The fetch loop and the HTTP handlers both use
// it, so every access holds mu. GitHub calls run outside the lock.
type Server struct {
	mu        sync.Mutex
	state     *engine.State
	lastFetch time.Time
	fetchErr  error
	fetched   chan struct{} // closed and replaced when a fetch result arrives

	refresh  func()        // ask the poller to fetch immediately
	followUp time.Duration // how long after an action to fetch once more
	tmpl     *template.Template

	// works tracks the GitHub calls and timers started by the fetch loop, so
	// Run can wait for them. stop tells the timers to give up.
	works sync.WaitGroup
	stop  chan struct{}
}

// New builds a server on the switches saved in the config's data directory.
func New(cfg config.Config, client engine.Client, refresh func()) (*Server, error) {
	state, err := engine.New(cfg, client)
	if err != nil {
		return nil, err
	}

	tmpl, err := template.ParseFS(assets, "page.html")
	if err != nil {
		return nil, err
	}

	return &Server{
		state:    state,
		refresh:  refresh,
		followUp: 15 * time.Second,
		tmpl:     tmpl,
		fetched:  make(chan struct{}),
		stop:     make(chan struct{}),
	}, nil
}

// Run feeds every fetch result into the engine and starts the merges and
// re-runs automation asks for. It returns when ctx is done, after the GitHub
// calls it started have finished.
func (s *Server) Run(ctx context.Context, results <-chan github.FetchResult) error {
	defer s.works.Wait()
	defer close(s.stop)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case res, ok := <-results:
			if !ok {
				return nil
			}

			s.handleFetch(res)
		}
	}
}

func (s *Server) handleFetch(res github.FetchResult) {
	s.mu.Lock()

	// Wake whoever is waiting for fresh data, such as the Refresh button.
	close(s.fetched)
	s.fetched = make(chan struct{})

	if res.Err != nil {
		slog.Error("failed to fetch PRs", "error", res.Err)
		s.fetchErr = res.Err
		s.mu.Unlock()

		return
	}

	s.fetchErr = nil
	s.lastFetch = time.Now()

	// A PR taken over from someone else lives on their fork, where they can
	// push a commit nobody here has looked at. Nothing is here to look while
	// the server runs unattended, so it leaves those PRs to the terminal app.
	own := make([]pr.PR, 0, len(res.PRs))

	for _, p := range res.PRs {
		if p.TakenOverFrom == "" {
			own = append(own, p)
		}
	}

	// A failed save is logged by the engine. The switches still hold in memory.
	works, _ := s.state.Apply(own)
	works = append(works, s.state.AutoWorks()...)

	s.mu.Unlock()

	for _, work := range works {
		s.works.Go(func() {
			if s.finish(work()) {
				s.refreshSoon()
			}
		})
	}
}

// finish hands a result back to the engine. It reports whether the PR list is
// worth fetching again.
func (s *Server) finish(res engine.Result) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch res := res.(type) {
	case engine.MergeResult:
		s.state.MergeDone(res)

		return true
	case engine.RerunResult:
		s.state.RerunDone(res)

		return true
	case engine.AutoRerunResult:
		_ = s.state.AutoRerunDone(res)

		return res.Reran
	case engine.MergedCheckResult:
		s.state.MergedCheckDone(res)
	}

	return false
}

// refreshSoon fetches now and once more a little later, since GitHub takes a
// moment to show the effect of a merge or a re-run.
func (s *Server) refreshSoon() {
	s.refresh()

	s.works.Go(func() {
		select {
		case <-s.stop:
		case <-time.After(s.followUp):
			s.refresh()
		}
	})
}

// Handler returns the page and its actions.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// For health probes. The page itself is the wrong target: a prober such as
	// the kubelet reads only the first 10KB of an answer and then hangs up,
	// which shows up as a failed write for every probe once the page is larger.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /{$}", s.handlePage)
	mux.HandleFunc("GET /prs", s.handleList)
	mux.HandleFunc("POST /toggle", s.handleToggle)
	mux.HandleFunc("POST /condition", s.handleCondition)
	mux.HandleFunc("POST /hide", s.handleHide)
	mux.HandleFunc("POST /merge", s.handleMerge)
	mux.HandleFunc("POST /rerun", s.handleRerun)
	mux.HandleFunc("POST /refresh", s.handleRefresh)
	mux.Handle("GET /static/", http.FileServerFS(assets))
	// iOS also asks for the icon at this fixed address, whatever the page says.
	mux.HandleFunc("GET /apple-touch-icon.png", func(w http.ResponseWriter, r *http.Request) {
		http.ServeFileFS(w, r, assets, "static/apple-touch-icon.png")
	})

	// The page has no login, so a request that another website makes through
	// the owner's browser must not be able to start a merge.
	return http.NewCrossOriginProtection().Handler(logActions(mux))
}

// logActions logs what was asked for from the page. The page's own polling is
// left out, it would bury everything else.
func logActions(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			slog.Info("page action", "action", r.URL.Path, "pr", r.FormValue("key"), "switch", r.FormValue("kind"))
		}

		next.ServeHTTP(w, r)
	})
}

func (s *Server) handlePage(w http.ResponseWriter, _ *http.Request) {
	s.render(w, "page", flash{})
}

func (s *Server) handleList(w http.ResponseWriter, _ *http.Request) {
	s.render(w, "list", flash{})
}

// handleRefresh fetches now and answers with the fresh list, so the button
// shows its effect at once instead of on the page's next poll.
func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	fetched := s.fetched
	s.mu.Unlock()

	s.refresh()

	select {
	case <-fetched:
		s.render(w, "main", flash{Text: "Refreshed"})
	case <-r.Context().Done():
	case <-time.After(15 * time.Second):
		s.render(w, "main", flash{Text: "GitHub is slow to answer, the list will update when it does", Err: true})
	}
}

func (s *Server) handleToggle(w http.ResponseWriter, r *http.Request) {
	kind := engine.Kind(r.FormValue("kind"))
	if kind != engine.Automerge && kind != engine.Autorerun {
		http.Error(w, "unknown switch", http.StatusBadRequest)

		return
	}

	s.mu.Lock()

	p, err := s.find(r)
	if err != nil {
		s.mu.Unlock()
		s.render(w, "main", flash{Text: err.Error(), Err: true})

		return
	}

	enabled, work, err := s.state.Toggle(kind, p)
	s.mu.Unlock()

	if err != nil {
		s.render(w, "main", flash{Text: "Failed to save the switch: " + err.Error(), Err: true})

		return
	}

	if work != nil {
		s.works.Go(func() {
			if s.finish(work()) {
				s.refreshSoon()
			}
		})
	}

	state := "off"
	if enabled {
		state = "on"
	}

	s.render(w, "main", flash{Text: string(kind) + " " + state + " for " + p.RepoShort()})
}

// handleCondition sets one automerge condition of a PR to the value the page
// names: "approvals" to 1 (the repository's own rule), 2 or 3, "comments" to
// "any" or "resolved".
func (s *Server) handleCondition(w http.ResponseWriter, r *http.Request) {
	value := r.FormValue("value")

	s.mu.Lock()

	p, err := s.find(r)

	var work engine.Work

	if err == nil {
		switch r.FormValue("kind") {
		case "approvals":
			approvals, convErr := strconv.Atoi(value)
			if convErr != nil {
				err = errUnknownCondition

				break
			}

			work, err = s.state.SetMinApprovals(p, approvals)
		case "comments":
			switch {
			case value != "any" && value != "resolved":
				err = errUnknownCondition
			case (value == "resolved") != s.state.ResolvedOnly[p.RepoShort()]:
				work, err = s.state.ToggleResolvedOnly(p)
			}
		default:
			err = errUnknownCondition
		}
	}

	s.mu.Unlock()

	if err != nil {
		s.render(w, "main", flash{Text: err.Error(), Err: true})

		return
	}

	if work != nil {
		s.works.Go(func() {
			if s.finish(work()) {
				s.refreshSoon()
			}
		})
	}

	s.render(w, "main", flash{})
}

// handleHide hides a PR, or shows it again when it is hidden.
func (s *Server) handleHide(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()

	p, err := s.find(r)

	hidden := false
	if err == nil {
		hidden, err = s.state.ToggleHidden(p)
	}

	s.mu.Unlock()

	switch {
	case err != nil:
		s.render(w, "main", flash{Text: err.Error(), Err: true})
	case hidden:
		s.render(w, "main", flash{Text: "Hid " + p.RepoShort()})
	default:
		s.render(w, "main", flash{Text: p.RepoShort() + " is back in the list"})
	}
}

func (s *Server) handleMerge(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()

	p, err := s.find(r)
	if err == nil {
		err = s.state.CheckMerge(p)
	}

	var work engine.Work
	if err == nil {
		work = s.state.MergeWork(p, false)
	}

	s.mu.Unlock()

	if err != nil {
		s.render(w, "main", flash{Text: err.Error(), Err: true})

		return
	}

	res, _ := work().(engine.MergeResult)
	s.finish(res)
	s.refreshSoon()

	if res.Err != nil {
		s.render(w, "main", flash{Text: "Failed to merge " + p.RepoShort() + ": " + res.Err.Error(), Err: true})

		return
	}

	s.render(w, "main", flash{Text: "Merged " + p.RepoShort()})
}

func (s *Server) handleRerun(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()

	p, err := s.find(r)
	if err == nil {
		err = s.state.StartRerun(p)
	}

	var work engine.Work
	if err == nil {
		work = s.state.RerunWork(p)
	}

	s.mu.Unlock()

	if err != nil {
		s.render(w, "main", flash{Text: err.Error(), Err: true})

		return
	}

	res, _ := work().(engine.RerunResult)
	s.finish(res)
	s.refreshSoon()

	if res.Err != nil {
		s.render(w, "main", flash{Text: "Re-run failed: " + res.Err.Error(), Err: true})

		return
	}

	s.render(w, "main", flash{Text: "Re-running failed jobs on " + p.RepoShort()})
}

var (
	errUnknownCondition = errors.New("unknown automerge condition")

	errGone    = errors.New("that pull request is no longer in the list")
	errChanged = errors.New("that pull request got a new commit since this page loaded, look again")
)

// find returns the PR a form names, as of the latest fetch. The form carries
// the head commit the page showed: a page left open on a phone can be hours
// old, and an action must not land on a commit the owner has not seen. The
// caller holds mu.
func (s *Server) find(r *http.Request) (pr.PR, error) {
	key, sha := r.FormValue("key"), r.FormValue("sha")

	for _, p := range s.state.PRs {
		if p.RepoShort() != key {
			continue
		}

		if p.HeadSHA != sha {
			return pr.PR{}, errChanged
		}

		return p, nil
	}

	return pr.PR{}, errGone
}

func (s *Server) render(w http.ResponseWriter, name string, f flash) {
	data := s.snapshot()
	data.Flash = f

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	if err := s.tmpl.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("failed to render the page", "template", name, "error", err)
	}
}
