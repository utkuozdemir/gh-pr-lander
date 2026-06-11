package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/utkuozdemir/sidero-rebaser/internal/pr"
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

func TestCountMergeCommentsPaginatesAndRequiresUsername(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	client := &Client{
		token:    "token",
		username: "me",
		httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if req.URL.Query().Get("page") == "2" {
				body := `[{"body":"/m","created_at":"` + now.Add(-time.Hour).
					Format(time.RFC3339) +
					`","user":{"login":"me"}}]`

				return testJSONResponse(body, nil), nil
			}

			body := `[` +
				`{"body":"/m","created_at":"` + now.Format(time.RFC3339) + `","user":{"login":"me"}},` +
				`{"body":"/m","created_at":"` + now.Format(time.RFC3339) + `","user":{"login":"someone-else"}}` +
				`]`

			return testJSONResponse(body, map[string]string{
				"Link": `<https://api.github.com/repos/org/repo/issues/1/comments?per_page=100&page=2>; rel="next"`,
			}), nil
		})},
	}

	count, last, err := client.CountMergeComments(context.Background(), "org/repo", 1, "/m")
	if err != nil {
		t.Fatalf("CountMergeComments: %v", err)
	}

	if count != 2 {
		t.Fatalf("expected 2 matching comments, got %d", count)
	}

	if !last.Equal(now) {
		t.Fatalf("expected latest comment time %s, got %s", now, last)
	}

	client.username = ""
	if _, _, err := client.CountMergeComments(context.Background(), "org/repo", 1, "/m"); err == nil {
		t.Fatal("expected unresolved username error")
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

func TestComputeCIStatusTreatsUnfetchedChecksAsPending(t *testing.T) {
	status, summary := computeCIStatus(prNode{
		Commits: commitConn{Nodes: []commitNode{{
			Commit: commitDetail{StatusCheckRollup: &statusCheckRollup{
				Contexts: contextConn{
					TotalCount: 2,
					Nodes: []contextNode{{
						TypeName:   "CheckRun",
						Status:     "COMPLETED",
						Conclusion: "SUCCESS",
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
						TypeName:   "CheckRun",
						Status:     "COMPLETED",
						Conclusion: "SUCCESS",
					}},
				},
			}},
		}}},
	})

	if status != pr.CIRunning {
		t.Fatalf("expected CIRunning when rollup state is PENDING, got %v", status)
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
	return graphQLPRNodeWithArchived(repo, number, sha, false)
}

func graphQLArchivedPRNode(repo string, number int, sha string) string {
	return graphQLPRNodeWithArchived(repo, number, sha, true)
}

func graphQLPRNodeWithArchived(repo string, number int, sha string, archived bool) string {
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
		"repository":{"nameWithOwner":"` + repo + `","name":"` + parts[1] + `","isArchived":` + boolJSON(archived) + `,"owner":{"login":"` + parts[0] + `"}},
		"headRepository":{"nameWithOwner":"me/` + parts[1] + `","name":"` + parts[1] + `","owner":{"login":"me"}},
		"headRef":{"name":"branch","target":{"oid":"` + sha + `"}},
		"commits":{"nodes":[{"commit":{"oid":"` + sha + `","statusCheckRollup":null}}]},
		"reviews":{"nodes":[]}
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
