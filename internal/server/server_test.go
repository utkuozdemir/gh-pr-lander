package server

import (
	"context"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/utkuozdemir/gh-pr-lander/internal/config"
	"github.com/utkuozdemir/gh-pr-lander/internal/github"
	"github.com/utkuozdemir/gh-pr-lander/internal/hint"
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

// The page shows the conditions on every PR, so they can be set before
// automerge is turned on. Turning it on for a ready PR merges at once.
func TestConditionsOnThePage(t *testing.T) {
	p := readyPR(1, "aaa")
	p.Approvals = 1

	s, client := testServer(t, p)
	form := url.Values{"key": {"org/repo#1"}, "sha": {"aaa"}}

	form.Set("kind", "approvals")
	form.Set("value", "2")

	if _, body := post(t, s, "/condition", form, nil); !strings.Contains(body, `value="2" aria-pressed="true"`) {
		t.Fatalf("expected the condition to be set with automerge off, got %q", body)
	}

	form.Set("kind", "automerge")

	_, body := post(t, s, "/toggle", form, nil)
	s.works.Wait()

	if len(client.merges()) != 0 || !strings.Contains(body, "Automerge waits for: 1 more approval") {
		t.Fatalf("expected automerge to hold back the ready PR, merges %v, body %q", client.merges(), body)
	}

	p.Approvals = 2
	s.handleFetch(github.FetchResult{PRs: []pr.PR{p}})
	s.works.Wait()

	if len(client.merges()) != 1 {
		t.Fatalf("expected a merge once the second approval is there, got %v", client.merges())
	}
}

func TestConditionFormIsCheckedLikeTheOthers(t *testing.T) {
	p := readyPR(1, "aaa")
	p.ReviewThreads = 1

	s, client := testServer(t, p)
	form := url.Values{"key": {"org/repo#1"}, "sha": {"aaa"}, "kind": {"comments"}, "value": {"resolved"}}

	// A stale page must not change what automerge waits for.
	stale := url.Values{"key": {"org/repo#1"}, "sha": {"old"}, "kind": {"comments"}, "value": {"resolved"}}
	if _, body := post(t, s, "/condition", stale, nil); !strings.Contains(body, "new commit") {
		t.Fatalf("expected a stale page to be refused, got %q", body)
	}

	post(t, s, "/condition", form, nil)
	post(t, s, "/toggle", url.Values{"key": {"org/repo#1"}, "sha": {"aaa"}, "kind": {"automerge"}}, nil)
	s.works.Wait()

	if len(client.merges()) != 0 {
		t.Fatal("expected automerge to wait for the unresolved comment")
	}

	// A value the page never sends is refused. Read as "any", it would drop
	// the condition and merge over the comment.
	form.Set("value", "")

	if _, body := post(t, s, "/condition", form, nil); !strings.Contains(body, "unknown automerge condition") {
		t.Fatalf("expected an unknown value to be refused, got %q", body)
	}

	s.works.Wait()

	if len(client.merges()) != 0 {
		t.Fatal("expected no merge after a refused value")
	}

	// Dropping the condition merges the ready PR right away.
	form.Set("value", "any")
	post(t, s, "/condition", form, nil)
	s.works.Wait()

	if len(client.merges()) != 1 {
		t.Fatalf("expected a merge once the condition is dropped, got %v", client.merges())
	}
}

func TestReviewMarkShowsApprovalsInEveryState(t *testing.T) {
	for status, want := range map[pr.ReviewStatus]string{
		pr.ReviewApproved:         "approved (2)",
		pr.ReviewPending:          "pending (2)",
		pr.ReviewChangesRequested: "changes requested (2)",
		pr.ReviewNone:             "approved (2)",
	} {
		if got := reviewMark(pr.PR{ReviewStatus: status, Approvals: 2}).Text; got != want {
			t.Fatalf("status %v: expected %q, got %q", status, want, got)
		}
	}

	if got := reviewMark(pr.PR{ReviewStatus: pr.ReviewNone}).Text; got != "none" {
		t.Fatalf("expected %q with no approvals, got %q", "none", got)
	}
}

func TestHealthzAnswersWithoutThePage(t *testing.T) {
	s, _ := testServer(t, readyPR(1, "aaa"))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK || rec.Body.String() != "ok\n" {
		t.Fatalf("expected a short ok, got %d %q", rec.Code, rec.Body.String())
	}
}

func TestPageShowsTheRunningVersion(t *testing.T) {
	s, _ := testServer(t)

	s.mu.Lock()
	s.state.Config.Version = "v1.2.3 (abc1234)"
	s.mu.Unlock()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), "gh-pr-lander v1.2.3 (abc1234)") {
		t.Fatal("expected the page to show which version is running")
	}
}

func TestPageShowsResolvedComments(t *testing.T) {
	p := readyPR(1, "aaa")
	p.ReviewThreads, p.ResolvedReviewThreads = 3, 1

	s, _ := testServer(t, p, readyPR(2, "bbb"))

	data := s.snapshot()
	if got := data.PRs[0].Comments; got.Text != "1/3 resolved" || got.Tone != toneWarn {
		t.Fatalf("expected the open comments to stand out, got %+v", got)
	}

	if got := data.PRs[1].Comments.Text; got != "" {
		t.Fatalf("expected nothing for a PR with no comments, got %q", got)
	}

	p.ResolvedReviewThreads = 3
	s.handleFetch(github.FetchResult{PRs: []pr.PR{p}})

	if got := s.snapshot().PRs[0].Comments; got.Text != "3/3 resolved" || got.Tone != toneOk {
		t.Fatalf("expected fully resolved comments to read as fine, got %+v", got)
	}
}

// The version sits in the part of the page that polls, so a page left open
// across an upgrade of the server shows the new one.
func TestListCarriesTheVersionAndThePRCount(t *testing.T) {
	s, _ := testServer(t, readyPR(1, "aaa"), readyPR(2, "bbb"))

	s.mu.Lock()
	s.state.Config.Version = "v1.2.3 (abc1234)"
	s.mu.Unlock()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/prs", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, "gh-pr-lander v1.2.3 (abc1234)") || !strings.Contains(body, "· 2 PRs") {
		t.Fatalf("expected the version and the PR count in the list, got %q", body)
	}
}

func TestAgesCarryTheirSecondsForThePageToKeepCounting(t *testing.T) {
	for d, want := range map[time.Duration]string{
		42 * time.Second:            "42s ago",
		95 * time.Second:            "1m 35s ago",
		2*time.Hour + 5*time.Minute: "2h 5m ago",
		-3 * time.Second:            "0s ago", // a clock that moved back
	} {
		got := ago(time.Now().Add(-d))
		if got.Text != want || got.Seconds != max(int(d.Seconds()), 0) {
			t.Fatalf("%s: expected %q, got %+v", d, want, got)
		}
	}

	s, _ := testServer(t, readyPR(1, "aaa"))

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/prs", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	if !strings.Contains(rec.Body.String(), `data-age="0"`) {
		t.Fatalf("expected the list to carry the age in seconds, got %q", rec.Body.String())
	}
}

// html/template checks a field name only when its branch runs, so a typo in a
// branch no test reaches would only show up on the live page.
func TestListRendersEveryBranchWithItsHoverText(t *testing.T) {
	failed := readyPR(1, "aaa")
	failed.CIStatus, failed.OverallStatus = pr.CIFailed, pr.StatusFailed
	failed.ReviewThreads = 2

	optional := readyPR(2, "bbb")
	optional.CIStatus = pr.CIPassed
	optional.Checks = pr.CheckSummary{Total: 3, Passed: 2, Running: 1, Optional: 1}

	s, _ := testServer(t, failed, optional, readyPR(3, "ccc"))

	// #3 leaves the list, and both give-up states are set on #1.
	s.handleFetch(github.FetchResult{PRs: []pr.PR{failed, optional}})
	s.works.Wait()

	s.mu.Lock()
	s.state.AutoMerge["org/repo#1"], s.state.MergeFailed["org/repo#1"] = true, true
	s.state.AutoRerun["org/repo#1"], s.state.RerunFailed["org/repo#1"] = true, true
	s.mu.Unlock()

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/prs", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, want := range []string{
		hint.Rerun, hint.RerunNothing, hint.Merge, hint.MergeNotReady, hint.ChecksOptional,
		hint.MergeGaveUp, hint.RerunGaveUp, hint.CommentCount, hint.Automerge, hint.Version,
		"Recently merged or closed",
	} {
		if !strings.Contains(body, template.HTMLEscapeString(want)) {
			t.Fatalf("expected the list to contain %q", want)
		}
	}

	// The template ran to its end.
	if !strings.Contains(body, "</footer>") {
		t.Fatal("expected the list to be complete")
	}
}

// iOS does not use an SVG icon for a bookmark or the home screen. It wants a
// PNG, named by the page or found at a fixed address.
func TestPageOffersAnIconForIOS(t *testing.T) {
	s, _ := testServer(t)

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)

		return rec
	}

	if !strings.Contains(get("/").Body.String(), `rel="apple-touch-icon" href="/static/apple-touch-icon.png"`) {
		t.Fatal("expected the page to name the icon for iOS")
	}

	for _, path := range []string{"/static/apple-touch-icon.png", "/apple-touch-icon.png", "/static/favicon-32.png"} {
		rec := get(path)
		if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" {
			t.Fatalf("%s: expected a PNG, got %d %q", path, rec.Code, rec.Header().Get("Content-Type"))
		}
	}
}

func TestPageShowsNotesNextToTheCommentCount(t *testing.T) {
	both := readyPR(1, "aaa")
	both.ReviewThreads, both.ResolvedReviewThreads, both.Notes = 2, 1, 1

	onlyNotes := readyPR(2, "bbb")
	onlyNotes.ReviewThreads, onlyNotes.Notes = 2, 2

	s, _ := testServer(t, both, onlyNotes)

	data := s.snapshot()
	if got := data.PRs[0]; got.Comments.Text != "1/1 resolved" || got.Comments.Tone != toneOk ||
		got.Notes.Text != "1 note" {
		t.Fatalf("expected a green 1/1 and one note, got %+v and %+v", got.Comments, got.Notes)
	}

	if got := data.PRs[1]; got.Comments.Text != "" || got.Notes.Text != "2 notes" {
		t.Fatalf("expected only the notes, got %+v and %+v", got.Comments, got.Notes)
	}

	req := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/prs", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)

	body := strings.Join(strings.Fields(rec.Body.String()), " ")
	if !strings.Contains(body, `1/1 resolved</span> · <span class="none"`) ||
		!strings.Contains(body, ">2 notes</span>") {
		t.Fatalf("expected the count and the notes on the page, got %q", body)
	}
}
