package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/utkuozdemir/gh-pr-lander/internal/config"
	"github.com/utkuozdemir/gh-pr-lander/internal/github"
	"github.com/utkuozdemir/gh-pr-lander/internal/pr"
)

// fakeClient records the merges asked of it.
type fakeClient struct {
	mu     sync.Mutex
	merged []string
}

func (f *fakeClient) FastForwardMerge(_ context.Context, repo, base, sha string) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.merged = append(f.merged, repo+" "+base+" "+sha)

	return nil
}

func (f *fakeClient) merges() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.merged...)
}

func (f *fakeClient) FailedWorkflowRunsSummary(context.Context, string, string) (github.WorkflowSummary, error) {
	return github.WorkflowSummary{}, nil
}
func (f *fakeClient) RerunFailedJobs(context.Context, string, string) error     { return nil }
func (f *fakeClient) ApproveWorkflowRuns(context.Context, string, string) error { return nil }
func (f *fakeClient) GetPRState(context.Context, string, int) (github.PRState, error) {
	return github.PRState{}, nil
}
func (f *fakeClient) DeleteBranch(context.Context, string, string) error { return nil }
func (f *fakeClient) Username() string                                   { return "me" }

func readyPR(number int, sha string) pr.PR {
	return pr.PR{
		Repo: "org/repo", Number: number, Title: "a title", BaseBranch: "main", HeadSHA: sha,
		OverallStatus: pr.StatusReady,
	}
}

func testServer(t *testing.T, prs ...pr.PR) (*Server, *fakeClient) {
	t.Helper()

	client := &fakeClient{}

	s, err := New(config.Config{
		DataDir:               t.TempDir(),
		PollInterval:          2 * time.Minute,
		AutoCooldown:          time.Hour,
		MergedDisplayDuration: time.Hour,
	}, client, func() {})
	if err != nil {
		t.Fatal(err)
	}

	s.followUp = time.Millisecond
	s.handleFetch(github.FetchResult{PRs: prs})

	t.Cleanup(func() {
		close(s.stop)
		s.works.Wait()
	})

	return s, client
}

func post(t *testing.T, s *Server, path string, form url.Values, headers map[string]string) (int, string) {
	t.Helper()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	return rec.Code, rec.Body.String()
}

func TestMergeNowMergesTheCommitThePageShowed(t *testing.T) {
	s, client := testServer(t, readyPR(1, "aaa"))

	// The page was loaded when the head was another commit.
	_, body := post(t, s, "/merge", url.Values{"key": {"org/repo#1"}, "sha": {"old"}}, nil)
	if len(client.merges()) != 0 || !strings.Contains(body, "new commit") {
		t.Fatalf("expected a stale page to be refused, merges %v, body %q", client.merges(), body)
	}

	_, body = post(t, s, "/merge", url.Values{"key": {"org/repo#1"}, "sha": {"aaa"}}, nil)
	if got := client.merges(); len(got) != 1 || got[0] != "org/repo main aaa" {
		t.Fatalf("expected one merge of the shown commit, got %v", got)
	}

	if !strings.Contains(body, "Merged org/repo#1") {
		t.Fatalf("expected the page to report the merge, got %q", body)
	}
}

func TestMergeNowRefusesAPRThatIsNotReady(t *testing.T) {
	p := readyPR(1, "aaa")
	p.OverallStatus = pr.StatusPending

	s, client := testServer(t, p)

	_, body := post(t, s, "/merge", url.Values{"key": {"org/repo#1"}, "sha": {"aaa"}}, nil)
	if len(client.merges()) != 0 || !strings.Contains(body, "not ready") {
		t.Fatalf("expected a refusal, merges %v, body %q", client.merges(), body)
	}
}

func TestRequestsFromOtherWebsitesAreRefused(t *testing.T) {
	s, client := testServer(t, readyPR(1, "aaa"))

	code, _ := post(t, s, "/merge", url.Values{"key": {"org/repo#1"}, "sha": {"aaa"}},
		map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://elsewhere.example"})
	if code != http.StatusForbidden || len(client.merges()) != 0 {
		t.Fatalf("expected 403 and no merge, got %d and %v", code, client.merges())
	}
}

func TestAutomergeTurnedOnFromThePageMergesAReadyPR(t *testing.T) {
	s, client := testServer(t, readyPR(1, "aaa"))

	_, body := post(t, s, "/toggle", url.Values{"key": {"org/repo#1"}, "sha": {"aaa"}, "kind": {"automerge"}}, nil)
	if !strings.Contains(body, "automerge on") {
		t.Fatalf("expected automerge to be on, got %q", body)
	}

	s.works.Wait()

	if got := client.merges(); len(got) != 1 {
		t.Fatalf("expected automerge to merge the ready PR, got %v", got)
	}
}

func TestAutomergeMergesWhenAFetchShowsThePRReady(t *testing.T) {
	pending := readyPR(1, "aaa")
	pending.OverallStatus = pr.StatusPending

	s, client := testServer(t, pending)

	post(t, s, "/toggle", url.Values{"key": {"org/repo#1"}, "sha": {"aaa"}, "kind": {"automerge"}}, nil)
	s.works.Wait()

	if len(client.merges()) != 0 {
		t.Fatal("expected no merge while the PR is pending")
	}

	s.handleFetch(github.FetchResult{PRs: []pr.PR{readyPR(1, "aaa")}})
	s.works.Wait()

	if got := client.merges(); len(got) != 1 {
		t.Fatalf("expected a merge once the PR is ready, got %v", got)
	}
}

func TestTakenOverPRsAreLeftToTheTerminalApp(t *testing.T) {
	theirs := readyPR(2, "bbb")
	theirs.TakenOverFrom = "someone"

	s, _ := testServer(t, readyPR(1, "aaa"), theirs)

	data := s.snapshot()
	if len(data.PRs) != 1 || data.PRs[0].Number != 1 {
		t.Fatalf("expected only the owner's PR on the page, got %+v", data.PRs)
	}
}

// The list refreshes itself in place, and htmx hands that target down to
// everything inside it. A form in the list that did not name its own target
// would put the whole answer, header included, where the list was.
func TestFormsInTheListNameTheirOwnTarget(t *testing.T) {
	s, _ := testServer(t, readyPR(1, "aaa"))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/prs", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	forms := strings.Count(body, "<form ")

	if forms == 0 || forms != strings.Count(body, `hx-target="#main" hx-swap="innerHTML"`) {
		t.Fatalf("expected each of the %d forms in the list to target #main, got %q", forms, body)
	}
}

func TestRefreshAnswersWithTheFreshList(t *testing.T) {
	s, _ := testServer(t, readyPR(1, "aaa"))

	// The poller answers a refresh with a new fetch result.
	s.refresh = func() {
		s.works.Go(func() {
			s.handleFetch(github.FetchResult{PRs: []pr.PR{readyPR(1, "aaa"), readyPR(2, "bbb")}})
		})
	}

	_, body := post(t, s, "/refresh", nil, nil)
	if !strings.Contains(body, "Refreshed") || !strings.Contains(body, "#2</b>") {
		t.Fatalf("expected the answer to show the newly fetched PR, got %q", body)
	}
}
