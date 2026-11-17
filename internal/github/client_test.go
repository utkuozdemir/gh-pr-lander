package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/utkuozdemir/gh-pr-lander/internal/pr"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testJSONResponse(body string, headers map[string]string) *http.Response {
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	for k, v := range headers {
		resp.Header.Set(k, v)
	}

	return resp
}

func TestFetchPRsQueriesEachOrgAndPaginates(t *testing.T) {
	client := &Client{
		token:    "token",
		username: "me",
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Path != "/graphql" {
				t.Fatalf("unexpected request path: %s", req.URL.Path)
			}

			var payload struct {
				Variables map[string]any `json:"variables"`
			}
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
				t.Fatalf("decode request: %v", err)
			}

			query, _ := payload.Variables["query"].(string)
			after := payload.Variables["after"]

			switch {
			case strings.Contains(query, "org:org1") && after == nil:
				return testJSONResponse(
					graphQLPRPage(true, "cursor-2", graphQLPRNode("org1/repo", 1, "sha1")),
					nil,
				), nil
			case strings.Contains(query, "org:org1") && after == "cursor-2":
				return testJSONResponse(
					graphQLPRPage(false, "", graphQLPRNode("org1/repo", 2, "sha2")),
					nil,
				), nil
			case strings.Contains(query, "org:org2") && after == nil:
				return testJSONResponse(
					graphQLPRPage(false, "", graphQLPRNode("org2/repo", 3, "sha3")),
					nil,
				), nil
			default:
				return nil, fmt.Errorf("unexpected query/after: %q %#v", query, after)
			}
		})},
	}

	prs, err := client.FetchPRs(context.Background(), []string{"org1", "org2"})
	if err != nil {
		t.Fatalf("FetchPRs: %v", err)
	}

	if len(prs) != 3 {
		t.Fatalf("expected 3 PRs, got %d: %#v", len(prs), prs)
	}
}

func TestFetchPRsFiltersArchivedRepos(t *testing.T) {
	client := &Client{
		token:    "token",
		username: "me",
		httpClient: &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			body := graphQLPRPage(false, "",
				graphQLPRNode("org/live", 1, "sha1"),
				graphQLArchivedPRNode("org/archived", 2, "sha2"),
			)

			return testJSONResponse(body, nil), nil
		})},
	}

	prs, err := client.FetchPRs(context.Background(), []string{"org"})
	if err != nil {
		t.Fatalf("FetchPRs: %v", err)
	}

	if len(prs) != 1 {
		t.Fatalf("expected only non-archived PR, got %d: %#v", len(prs), prs)
	}

	if prs[0].Repo != "org/live" {
		t.Fatalf("expected live repo PR, got %s", prs[0].Repo)
	}
}

func TestFetchPRsKeepsOnlyPRsTheUserTouched(t *testing.T) {
	client := &Client{
		token:    "token",
		username: "me",
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			var payload struct {
				Variables map[string]any `json:"variables"`
			}
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
				t.Fatalf("decode request: %v", err)
			}

			if query, _ := payload.Variables["query"].(string); !strings.Contains(query, "involves:@me") {
				t.Fatalf("expected an involves:@me search, got %q", query)
			}

			body := graphQLPRPage(false, "",
				graphQLPRNode("org/repo", 1, "sha1"),
				// Only commented on: the commits are all someone else's.
				graphQLForeignPRNode("org/repo", 2, "other", [2]string{"other", "other"}, [2]string{"", ""}),
				// Rebased and re-signed: the user is the committer.
				graphQLForeignPRNode("org/repo", 3, "other", [2]string{"other", "Me"}),
				// The user added a commit that is no longer the head.
				graphQLForeignPRNode("org/repo", 4, "other", [2]string{"me", "me"}, [2]string{"other", "other"}),
			)

			return testJSONResponse(body, nil), nil
		})},
	}

	prs, err := client.FetchPRs(context.Background(), []string{"org"})
	if err != nil {
		t.Fatalf("FetchPRs: %v", err)
	}

	got := map[int]string{}
	for _, p := range prs {
		got[p.Number] = p.TakenOverFrom
	}

	want := map[int]string{1: "", 3: "other", 4: "other"}
	if !maps.Equal(got, want) {
		t.Fatalf("expected PRs (number -> taken over from) %v, got %v", want, got)
	}
}

func TestFailedWorkflowRunsSummaryPaginates(t *testing.T) {
	first := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)
	second := first.Add(time.Hour)
	client := &Client{
		token: "token",
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Query().Get("page") == "2" {
				body := `{"workflow_runs":[{"id":2,"run_attempt":4,"conclusion":"timed_out","updated_at":"` + second.Format(
					time.RFC3339,
				) + `"}]}`

				return testJSONResponse(body, nil), nil
			}

			body := `{"workflow_runs":[{"id":1,"run_attempt":2,"conclusion":"failure","updated_at":"` + first.Format(
				time.RFC3339,
			) + `"},{"id":3,"run_attempt":1,"conclusion":"success","updated_at":"` + first.Format(
				time.RFC3339,
			) + `"}]}`

			return testJSONResponse(body, map[string]string{
				"Link": `<https://api.github.com/repos/org/repo/actions/runs?head_sha=sha&per_page=100&page=2>; rel="next"`,
			}), nil
		})},
	}

	summary, err := client.FailedWorkflowRunsSummary(context.Background(), "org/repo", "sha")
	if err != nil {
		t.Fatalf("FailedWorkflowRunsSummary: %v", err)
	}

	if summary.Count != 2 {
		t.Fatalf("expected 2 failed runs, got %d", summary.Count)
	}

	if summary.MaxAttempt != 4 {
		t.Fatalf("expected max attempt 4, got %d", summary.MaxAttempt)
	}

	if !summary.LatestUpdatedAt.Equal(second) {
		t.Fatalf("expected latest update %s, got %s", second, summary.LatestUpdatedAt)
	}
}

func TestFailedWorkflowRunsSummaryCountsRunningRuns(t *testing.T) {
	client := &Client{
		token: "token",
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			// A run with a failed job but other jobs still going reports no
			// conclusion yet, so it is running, not failed.
			body := `{"workflow_runs":[` +
				`{"id":1,"status":"in_progress","conclusion":null},` +
				`{"id":2,"status":"completed","conclusion":"success"}]}`

			return testJSONResponse(body, nil), nil
		})},
	}

	summary, err := client.FailedWorkflowRunsSummary(context.Background(), "org/repo", "sha")
	if err != nil {
		t.Fatalf("FailedWorkflowRunsSummary: %v", err)
	}

	if summary.Count != 0 || summary.Running != 1 {
		t.Fatalf("expected 0 failed and 1 running, got %d failed and %d running", summary.Count, summary.Running)
	}
}

func TestFailedWorkflowRunsSummaryCountsRunsAwaitingApproval(t *testing.T) {
	client := &Client{
		token: "token",
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			// A fork's workflow waits for a maintainer to approve it.
			body := `{"workflow_runs":[{"id":1,"status":"completed","conclusion":"action_required"}]}`

			return testJSONResponse(body, nil), nil
		})},
	}

	summary, err := client.FailedWorkflowRunsSummary(context.Background(), "org/repo", "sha")
	if err != nil {
		t.Fatalf("FailedWorkflowRunsSummary: %v", err)
	}

	if summary.AwaitingApproval != 1 || summary.Count != 0 {
		t.Fatalf("expected 1 awaiting approval and 0 failed, got %d and %d", summary.AwaitingApproval, summary.Count)
	}
}

func TestApproveWorkflowRunsApprovesOnlyRunsAwaitingApproval(t *testing.T) {
	var posted []string

	client := &Client{
		token: "token",
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.Method == http.MethodPost {
				posted = append(posted, req.URL.Path)

				return &http.Response{StatusCode: http.StatusCreated, Body: http.NoBody}, nil
			}

			body := `{"workflow_runs":[` +
				`{"id":1,"status":"completed","conclusion":"action_required"},` +
				`{"id":2,"status":"completed","conclusion":"failure"}]}`

			return testJSONResponse(body, nil), nil
		})},
	}

	if err := client.ApproveWorkflowRuns(context.Background(), "org/repo", "sha"); err != nil {
		t.Fatalf("ApproveWorkflowRuns: %v", err)
	}

	if len(posted) != 1 || posted[0] != "/repos/org/repo/actions/runs/1/approve" {
		t.Fatalf("unexpected approvals: %v", posted)
	}
}

func TestComputeCIStatusTreatsUnfetchedChecksAsPending(t *testing.T) {
	status, summary := computeCIStatus(prNode{
		Commits: commitConn{Nodes: []commitNode{{
			Commit: commitDetail{StatusCheckRollup: &statusCheckRollup{
				Contexts: contextConn{
					TotalCount: 2,
					Nodes: []contextNode{{
						TypeName:   typeCheckRun,
						Status:     checkCompleted,
						Conclusion: stateSuccess,
					}},
				},
			}},
		}}},
	})

	if status != pr.CIRunning {
		t.Fatalf("expected CIRunning for unfetched checks, got %v", status)
	}

	if summary.Total != 2 || summary.Pending != 1 || summary.Passed != 1 {
		t.Fatalf("unexpected summary: %#v", summary)
	}
}

func TestComputeCIStatusHonorsPendingRollupState(t *testing.T) {
	// Every fetched context passed, but the rollup aggregate still says checks
	// are expected (a fresh commit whose required checks have not registered).
	status, _ := computeCIStatus(prNode{
		Commits: commitConn{Nodes: []commitNode{{
			Commit: commitDetail{StatusCheckRollup: &statusCheckRollup{
				State: "PENDING",
				Contexts: contextConn{
					TotalCount: 1,
					Nodes: []contextNode{{
						TypeName:   typeCheckRun,
						Status:     checkCompleted,
						Conclusion: stateSuccess,
					}},
				},
			}},
		}}},
	})

	if status != pr.CIRunning {
		t.Fatalf("expected CIRunning when rollup state is PENDING, got %v", status)
	}
}

func TestPRFromNodeCountsReviewThreads(t *testing.T) {
	// 3 threads exist but only 2 were fetched, the unfetched one counts as
	// unresolved.
	p := prFromNode(prNode{ReviewThreads: threadConn{
		TotalCount: 3,
		Nodes:      []threadNode{{IsResolved: true}, {IsResolved: true}},
	}})

	if p.ReviewThreads != 3 || p.ResolvedReviewThreads != 2 {
		t.Fatalf("expected 2/3 resolved threads, got %d/%d", p.ResolvedReviewThreads, p.ReviewThreads)
	}
}

func TestPRFromNodeDetectsUnenforcedBehind(t *testing.T) {
	// GitHub reports CLEAN when the base branch does not require being up to
	// date, even though main moved past the PR's base.
	node := prNode{
		MergeStateStatus: "CLEAN",
		BaseRefOID:       "old",
		BaseRef:          &refNode{Target: targetNode{OID: "new"}},
	}

	p := prFromNode(node)
	if p.BaseStatus != pr.BaseBehind || !p.BehindUnenforced {
		t.Fatalf("expected unenforced behind, got status %v unenforced %v", p.BaseStatus, p.BehindUnenforced)
	}

	if p.OverallStatus != pr.StatusStale {
		t.Fatalf("expected StatusStale, got %v", p.OverallStatus)
	}

	node.MergeStateStatus = "UNKNOWN"
	node.Mergeable = "MERGEABLE"

	p = prFromNode(node)
	if p.BaseStatus != pr.BaseBehind || p.BehindUnenforced {
		t.Fatalf(
			"expected behind with enforcement unknown, got status %v unenforced %v",
			p.BaseStatus,
			p.BehindUnenforced,
		)
	}

	// Just fast-forward merged and still listed as open: the base branch is on
	// the PR's own head commit, which is not behind.
	node.MergeStateStatus = "CLEAN"
	node.HeadRef = refNode{Target: targetNode{OID: "new"}}

	p = prFromNode(node)
	if p.BaseStatus != pr.BaseSynced {
		t.Fatalf("expected a just merged PR to stay synced, got status %v", p.BaseStatus)
	}

	node.BaseRef.Target.OID = "old"

	p = prFromNode(node)
	if p.BaseStatus != pr.BaseSynced || p.BehindUnenforced {
		t.Fatalf("expected synced, got status %v unenforced %v", p.BaseStatus, p.BehindUnenforced)
	}
}

func graphQLPRPage(hasNext bool, cursor string, nodes ...string) string {
	return `{"data":{"search":{"pageInfo":{"hasNextPage":` + boolJSON(
		hasNext,
	) + `,"endCursor":"` + cursor + `"},"nodes":[` + strings.Join(
		nodes,
		",",
	) + `]}}}`
}

func graphQLPRNode(repo string, number int, sha string) string {
	return graphQLPRNodeFull(repo, number, sha, false, "me", `{"nodes":[]}`)
}

func graphQLArchivedPRNode(repo string, number int, sha string) string {
	return graphQLPRNodeFull(repo, number, sha, true, "me", `{"nodes":[]}`)
}

// graphQLForeignPRNode is a PR opened by author, whose commits were authored and
// committed by the given logins ("" for a commit email not linked to a user).
func graphQLForeignPRNode(repo string, number int, author string, commits ...[2]string) string {
	actor := func(login string) string {
		if login == "" {
			return `{"user":null}`
		}

		return `{"user":{"login":"` + login + `"}}`
	}

	nodes := make([]string, len(commits))
	for i, c := range commits {
		nodes[i] = `{"commit":{"author":` + actor(c[0]) + `,"committer":` + actor(c[1]) + `}}`
	}

	return graphQLPRNodeFull(repo, number, "sha", false, author, `{"nodes":[`+strings.Join(nodes, ",")+`]}`)
}

func graphQLPRNodeFull(repo string, number int, sha string, archived bool, author, allCommits string) string {
	parts := strings.Split(repo, "/")

	return `{
		"number":` + intJSON(number) + `,
		"title":"test",
		"url":"https://github.com/` + repo + `/pull/` + intJSON(number) + `",
		"state":"OPEN",
		"isDraft":false,
		"baseRefName":"main",
		"headRefName":"branch",
		"mergeable":"MERGEABLE",
		"mergeStateStatus":"CLEAN",
		"reviewDecision":"",
		"author":{"login":"` + author + `"},
		"repository":{"nameWithOwner":"` + repo + `","name":"` + parts[1] + `","isArchived":` + boolJSON(archived) + `,"owner":{"login":"` + parts[0] + `"}},
		"headRepository":{"nameWithOwner":"me/` + parts[1] + `","name":"` + parts[1] + `","owner":{"login":"me"}},
		"headRef":{"name":"branch","target":{"oid":"` + sha + `"}},
		"commits":{"nodes":[{"commit":{"oid":"` + sha + `","statusCheckRollup":null}}]},
		"latestOpinionatedReviews":{"nodes":[]},
		"allCommits":` + allCommits + `
	}`
}

func boolJSON(v bool) string {
	if v {
		return "true"
	}

	return "false"
}

func intJSON(v int) string {
	return strconv.Itoa(v)
}

func TestFastForwardMergeMovesBaseBranchWithoutForce(t *testing.T) {
	var gotMethod, gotPath, gotBody string

	status := http.StatusOK
	client := &Client{
		token: "token",
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body, _ := io.ReadAll(req.Body)
			gotMethod, gotPath, gotBody = req.Method, req.URL.Path, string(body)

			resp := testJSONResponse(`{"message":"Update is not a fast forward"}`, nil)
			resp.StatusCode = status

			return resp, nil
		})},
	}

	if err := client.FastForwardMerge(context.Background(), "org/repo", "release-1.0", "abc123"); err != nil {
		t.Fatalf("FastForwardMerge: %v", err)
	}

	if gotMethod != http.MethodPatch || gotPath != "/repos/org/repo/git/refs/heads/release-1.0" {
		t.Fatalf("unexpected request %s %s", gotMethod, gotPath)
	}

	if gotBody != `{"force":false,"sha":"abc123"}` {
		t.Fatalf("unexpected request body %s", gotBody)
	}

	status = http.StatusUnprocessableEntity

	err := client.FastForwardMerge(context.Background(), "org/repo", "main", "abc123")
	if !errors.Is(err, ErrNotFastForward) {
		t.Fatalf("expected ErrNotFastForward, got %v", err)
	}

	status = http.StatusBadGateway

	err = client.FastForwardMerge(context.Background(), "org/repo", "main", "abc123")
	if err == nil || errors.Is(err, ErrMergeRefused) {
		t.Fatalf("expected a server error that is not a refusal, got %v", err)
	}
}

func TestFetchPRsWithNoOrgsSearchesEverywhere(t *testing.T) {
	var queries []string

	client := &Client{
		token:    "token",
		username: "me",
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			var payload struct {
				Variables map[string]any `json:"variables"`
			}
			if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
				t.Fatalf("decode request: %v", err)
			}

			query, _ := payload.Variables["query"].(string)
			queries = append(queries, query)

			return testJSONResponse(graphQLPRPage(false, "", graphQLPRNode("any/repo", 1, "sha1")), nil), nil
		})},
	}

	prs, err := client.FetchPRs(context.Background(), nil)
	if err != nil {
		t.Fatalf("FetchPRs: %v", err)
	}

	if len(prs) != 1 || len(queries) != 1 || strings.Contains(queries[0], "org:") {
		t.Fatalf("expected one search with no org qualifier, got %d PRs and queries %q", len(prs), queries)
	}
}

func TestComputeCIStatusDoesNotWaitForOptionalChecks(t *testing.T) {
	node := prNode{}
	node.Commits.Nodes = []commitNode{{}}
	node.Commits.Nodes[0].Commit.StatusCheckRollup = &statusCheckRollup{State: "PENDING"}
	rollup := node.Commits.Nodes[0].Commit.StatusCheckRollup
	rollup.Contexts.TotalCount = 2
	rollup.Contexts.Nodes = []contextNode{
		{TypeName: typeCheckRun, Status: checkCompleted, Conclusion: stateSuccess},
		{TypeName: typeCheckRun, Status: "IN_PROGRESS"},
	}

	// Without GitHub's word that the open check is not required, wait for it.
	node.MergeStateStatus = "BLOCKED"

	if status, _ := computeCIStatus(node); status != pr.CIRunning {
		t.Fatalf("expected CIRunning while a check that may be required runs, got %v", status)
	}

	// UNSTABLE: mergeable now, the check still running is not required.
	node.MergeStateStatus = "UNSTABLE"

	status, summary := computeCIStatus(node)
	if status != pr.CIPassed || summary.Optional != 1 {
		t.Fatalf("expected CIPassed with one optional check open, got %v and %+v", status, summary)
	}

	if got := summary.String(); got != "1/2 passed, 1 optional still running" {
		t.Fatalf("unexpected summary %q", got)
	}

	// A failed check still shows as failed, required or not.
	rollup.Contexts.Nodes[1] = contextNode{TypeName: typeCheckRun, Status: checkCompleted, Conclusion: "FAILURE"}

	if status, _ := computeCIStatus(node); status != pr.CIFailed {
		t.Fatalf("expected CIFailed for a failed check, got %v", status)
	}
}

func TestPRFromNodeCountsApprovals(t *testing.T) {
	p := prFromNode(prNode{LatestReviews: reviewConn{Nodes: []reviewNode{
		{State: "APPROVED"}, {State: "CHANGES_REQUESTED"}, {State: "APPROVED"},
	}}})

	if p.Approvals != 2 {
		t.Fatalf("expected 2 approvals, got %d", p.Approvals)
	}
}

func TestAnnotateNotesCountsThreadsOnlyTheUserWrote(t *testing.T) {
	thread := func(resolved bool, total int, authors ...string) string {
		nodes := make([]string, len(authors))
		for i, a := range authors {
			nodes[i] = `{"author":{"login":"` + a + `"}}`
		}

		return `{"isResolved":` + boolJSON(resolved) + `,"comments":{"totalCount":` + strconv.Itoa(total) +
			`,"nodes":[` + strings.Join(nodes, ",") + `]}}`
	}

	body := `{"data":{"repository":{"pullRequest":{"reviewThreads":{"nodes":[` + strings.Join([]string{
		thread(false, 1, "me"),             // a note
		thread(false, 2, "me", "Me"),       // still only the user, a note
		thread(false, 2, "me", "reviewer"), // someone replied, a conversation
		thread(false, 1, "reviewer"),       // a review comment
		thread(true, 1, "me"),              // resolved, not counted at all
		thread(false, 30, "me"),            // longer than what was looked at
	}, ",") + `]}}}}}`

	requests := 0
	client := &Client{
		token:    "token",
		username: "me",
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests++

			return testJSONResponse(body, nil), nil
		})},
	}

	own := pr.PR{
		Repo:                  "org/repo",
		RepoOwner:             "org",
		RepoName:              "repo",
		Number:                1,
		ReviewThreads:         6,
		ResolvedReviewThreads: 1,
	}
	theirs := own
	theirs.Number, theirs.TakenOverFrom = 2, "someone"
	resolved := own
	resolved.Number, resolved.ResolvedReviewThreads = 3, 6

	prs := []pr.PR{own, theirs, resolved}
	if err := client.annotateNotes(context.Background(), prs); err != nil {
		t.Fatal(err)
	}

	if prs[0].Notes != 2 || prs[0].UnresolvedThreads() != 3 || prs[0].CommentThreads() != 4 {
		t.Fatalf("expected 2 notes and 3 unresolved of 4 comments, got %d, %d of %d",
			prs[0].Notes, prs[0].UnresolvedThreads(), prs[0].CommentThreads())
	}

	// Someone else's PR and a PR with nothing unresolved are not asked about.
	if prs[1].Notes != 0 || prs[2].Notes != 0 || requests != 1 {
		t.Fatalf("expected one lookup and no notes elsewhere, got %d lookups, %d and %d notes",
			requests, prs[1].Notes, prs[2].Notes)
	}

	// The answer is kept until the PR changes.
	again := []pr.PR{own}
	if err := client.annotateNotes(context.Background(), again); err != nil || again[0].Notes != 2 || requests != 1 {
		t.Fatalf(
			"expected the kept answer and no new lookup, got %d notes, %d lookups, %v",
			again[0].Notes,
			requests,
			err,
		)
	}

	changed := own
	changed.UpdatedAt = time.Now()

	if err := client.annotateNotes(context.Background(), []pr.PR{changed}); err != nil || requests != 2 {
		t.Fatalf("expected a new lookup for a changed PR, got %d lookups, %v", requests, err)
	}
}

// A commit that failed once is asked about until it is replaced, so that its
// re-run count is known while the re-run runs and after it passed. A commit
// that passed first time is never asked about.
func TestAnnotateWorkflowRunsKeepsTheRerunCountAfterAFailure(t *testing.T) {
	requests := 0
	client := &Client{
		token: "token",
		httpClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			requests++
			// The most re-run workflow is on its third attempt, and it is not
			// the failed one.
			body := `{"workflow_runs":[` +
				`{"id":1,"status":"completed","conclusion":"failure","run_attempt":2},` +
				`{"id":2,"status":"in_progress","conclusion":null,"run_attempt":3}]}`

			return testJSONResponse(body, nil), nil
		})},
	}

	annotate := func(ci pr.CIStatus, sha string) pr.PR {
		prs := []pr.PR{
			{Repo: "org/repo", Number: 1, HeadSHA: sha, CIStatus: ci},
			{Repo: "org/repo", Number: 2, HeadSHA: "other", CIStatus: pr.CIRunning}, // never failed
		}
		if err := client.annotateWorkflowRuns(context.Background(), prs); err != nil {
			t.Fatal(err)
		}

		if prs[1].Reruns != 0 {
			t.Fatalf("expected no count for a commit that never failed, got %d", prs[1].Reruns)
		}

		return prs[0]
	}

	if got := annotate(pr.CIFailed, "aaa"); got.Reruns != 2 || requests != 1 {
		t.Fatalf("expected 2 re-runs after one lookup, got %d after %d", got.Reruns, requests)
	}

	// The re-run is running, then passed: still asked about.
	if got := annotate(pr.CIRunning, "aaa"); got.Reruns != 2 || requests != 2 {
		t.Fatalf("expected the count while the re-run runs, got %d after %d lookups", got.Reruns, requests)
	}

	if got := annotate(pr.CIPassed, "aaa"); got.Reruns != 2 || requests != 3 {
		t.Fatalf("expected the count after it passed, got %d after %d lookups", got.Reruns, requests)
	}

	// A new commit starts over, and is not asked about until it fails.
	if got := annotate(pr.CIRunning, "bbb"); got.Reruns != 0 || requests != 3 {
		t.Fatalf("expected no count and no lookup for a new commit, got %d after %d lookups", got.Reruns, requests)
	}
}

func TestChecksURLPicksTheRunWorthLookingAt(t *testing.T) {
	check := func(run, status, conclusion string) contextNode {
		node := contextNode{TypeName: typeCheckRun, Status: status, Conclusion: conclusion}
		if run != "" {
			node.CheckSuite = &checkSuiteNode{WorkflowRun: &workflowRunNode{URL: run}}
		}

		return node
	}

	passed := func(run string) contextNode { return check(run, checkCompleted, stateSuccess) }

	for _, tt := range []struct {
		name   string
		checks []contextNode
		want   string
	}{
		{
			"the run with the most checks when all passed",
			[]contextNode{passed("labeler"), passed("pipeline"), passed("pipeline"), passed("")},
			"pipeline",
		},
		{
			"a run that is still going before a larger one that passed",
			[]contextNode{passed("pipeline"), passed("pipeline"), check("docs", "IN_PROGRESS", "")},
			"docs",
		},
		{
			"a failed run before one that is still going",
			[]contextNode{check("pipeline", "QUEUED", ""), check("pipeline", "IN_PROGRESS", ""), check("lint", checkCompleted, "FAILURE")},
			"lint",
		},
		{
			"the Checks tab when no check belongs to a workflow run",
			[]contextNode{passed(""), {TypeName: "StatusContext", State: stateSuccess}},
			"https://github.com/org/repo/pull/1/checks",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := checksURL(prNode{
				URL: "https://github.com/org/repo/pull/1",
				Commits: commitConn{Nodes: []commitNode{
					{
						Commit: commitDetail{
							StatusCheckRollup: &statusCheckRollup{Contexts: contextConn{Nodes: tt.checks}},
						},
					},
				}},
			})
			if got != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
		})
	}

	if got := checksURL(
		prNode{URL: "https://github.com/org/repo/pull/1"},
	); got != "https://github.com/org/repo/pull/1/checks" {
		t.Fatalf("expected the Checks tab for a PR with no checks, got %q", got)
	}
}
