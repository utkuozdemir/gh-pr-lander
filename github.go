package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type GitHubClient struct {
	token      string
	httpClient *http.Client
	username   string // cached authenticated user login
}

func newGitHubClient(token string) *GitHubClient {
	return &GitHubClient{
		token:      token,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// resolveUsername fetches and caches the authenticated user's login.
// Call once at startup to avoid races from concurrent Bubble Tea commands.
func (c *GitHubClient) resolveUsername(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("GET /user HTTP %d: %s", resp.StatusCode, string(body))
	}

	var user struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return err
	}

	c.username = user.Login
	return nil
}

func (c *GitHubClient) getUsername() string {
	return c.username
}

// GraphQL response types

type graphQLResponse struct {
	Data   graphQLData    `json:"data"`
	Errors []graphQLError `json:"errors"`
}

type graphQLError struct {
	Message string `json:"message"`
}

type graphQLData struct {
	Search searchResult `json:"search"`
}

type searchResult struct {
	Nodes    []prNode `json:"nodes"`
	PageInfo pageInfo `json:"pageInfo"`
}

type pageInfo struct {
	HasNextPage bool   `json:"hasNextPage"`
	EndCursor   string `json:"endCursor"`
}

type prNode struct {
	ID               string     `json:"id"`
	Number           int        `json:"number"`
	Title            string     `json:"title"`
	URL              string     `json:"url"`
	State            string     `json:"state"`
	IsDraft          bool       `json:"isDraft"`
	BaseRefName      string     `json:"baseRefName"`
	HeadRefName      string     `json:"headRefName"`
	Mergeable        string     `json:"mergeable"`
	MergeStateStatus string     `json:"mergeStateStatus"`
	ReviewDecision   string     `json:"reviewDecision"`
	Repository       repoNode   `json:"repository"`
	HeadRef          refNode    `json:"headRef"`
	Commits          commitConn `json:"commits"`
	Reviews          reviewConn `json:"reviews"`
	HeadRepository   *repoNode  `json:"headRepository"`
}

type repoNode struct {
	NameWithOwner string    `json:"nameWithOwner"`
	Name          string    `json:"name"`
	Owner         ownerNode `json:"owner"`
	IsArchived    bool      `json:"isArchived"`
}

type ownerNode struct {
	Login string `json:"login"`
}

type refNode struct {
	Name   string     `json:"name"`
	Target targetNode `json:"target"`
}

type targetNode struct {
	OID string `json:"oid"`
}

type commitConn struct {
	Nodes []commitNode `json:"nodes"`
}

type commitNode struct {
	Commit commitDetail `json:"commit"`
}

type commitDetail struct {
	OID               string             `json:"oid"`
	StatusCheckRollup *statusCheckRollup `json:"statusCheckRollup"`
}

type statusCheckRollup struct {
	State    string      `json:"state"`
	Contexts contextConn `json:"contexts"`
}

type contextConn struct {
	TotalCount int           `json:"totalCount"`
	Nodes      []contextNode `json:"nodes"`
}

type contextNode struct {
	TypeName   string `json:"__typename"`
	Name       string `json:"name"`       // CheckRun
	Conclusion string `json:"conclusion"` // CheckRun
	Status     string `json:"status"`     // CheckRun
	Context    string `json:"context"`    // StatusContext
	State      string `json:"state"`      // StatusContext
}

type reviewConn struct {
	Nodes []reviewNode `json:"nodes"`
}

type reviewNode struct {
	State  string     `json:"state"`
	Author authorNode `json:"author"`
}

type authorNode struct {
	Login string `json:"login"`
}

const prQuery = `query($query: String!, $after: String) {
  search(query: $query, type: ISSUE, first: 100, after: $after) {
    pageInfo {
      hasNextPage
      endCursor
    }
    nodes {
      ... on PullRequest {
        id
        number
        title
        url
        state
        isDraft
        baseRefName
        headRefName
        mergeable
        mergeStateStatus
        reviewDecision
	        repository {
	          nameWithOwner
	          name
	          isArchived
	          owner { login }
	        }
        headRepository {
          nameWithOwner
          name
          owner { login }
        }
        headRef {
          name
          target { oid }
        }
        commits(last: 1) {
          nodes {
            commit {
              oid
              statusCheckRollup {
                state
                contexts(first: 100) {
                  totalCount
                  nodes {
                    __typename
                    ... on CheckRun {
                      name
                      conclusion
                      status
                    }
                    ... on StatusContext {
                      context
                      state
                    }
                  }
                }
              }
            }
          }
        }
        reviews(last: 20, states: [APPROVED, CHANGES_REQUESTED, DISMISSED]) {
          nodes {
            state
            author { login }
          }
        }
      }
    }
  }
}`

func (c *GitHubClient) graphql(ctx context.Context, query string, variables map[string]any, result any) error {
	body := map[string]any{
		"query":     query,
		"variables": variables,
	}

	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal graphql request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.github.com/graphql", bytes.NewReader(data))
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("graphql request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("read graphql response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("graphql HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return json.Unmarshal(respBody, result)
}

func (c *GitHubClient) fetchPRs(ctx context.Context, orgs []string) ([]PR, error) {
	seen := make(map[string]PR)

	for _, org := range orgs {
		orgPRs, err := c.fetchPRsForOrg(ctx, org)
		if err != nil {
			return nil, err
		}
		for _, pr := range orgPRs {
			seen[pr.RepoShort()] = pr
		}
	}

	prs := make([]PR, 0, len(seen))
	for _, pr := range seen {
		prs = append(prs, pr)
	}

	c.annotateFailedWorkflowRuns(ctx, prs)
	sortPRs(prs)

	return prs, nil
}

func (c *GitHubClient) fetchPRsForOrg(ctx context.Context, org string) ([]PR, error) {
	searchQuery := fmt.Sprintf("is:pr is:open author:@me org:%s", org)
	var prs []PR
	var after any

	for {
		variables := map[string]any{
			"query": searchQuery,
			"after": after,
		}

		var resp graphQLResponse
		if err := c.graphql(ctx, prQuery, variables, &resp); err != nil {
			return nil, err
		}

		if len(resp.Errors) > 0 {
			msgs := make([]string, len(resp.Errors))
			for i, e := range resp.Errors {
				msgs[i] = e.Message
			}
			return nil, fmt.Errorf("graphql errors for org %s: %s", org, strings.Join(msgs, "; "))
		}

		for _, node := range resp.Data.Search.Nodes {
			if node.Repository.IsArchived {
				continue
			}
			prs = append(prs, prFromNode(node))
		}

		page := resp.Data.Search.PageInfo
		if !page.HasNextPage || page.EndCursor == "" {
			break
		}
		after = page.EndCursor
	}

	return prs, nil
}

func (c *GitHubClient) annotateFailedWorkflowRuns(ctx context.Context, prs []PR) {
	for i := range prs {
		if prs[i].CIStatus != CIFailed || prs[i].HeadSHA == "" {
			continue
		}

		summary, err := c.failedWorkflowRunsSummary(ctx, prs[i].Repo, prs[i].HeadSHA)
		if err != nil {
			continue
		}

		prs[i].FailedWorkflowRunsKnown = true
		prs[i].FailedWorkflowRuns = summary.Count
		prs[i].MaxFailedRunAttempt = summary.MaxAttempt
		prs[i].LatestFailedRunUpdate = summary.LatestUpdatedAt
	}
}

// postCommentRaw posts a comment without any duplicate checking.
func (c *GitHubClient) postCommentRaw(ctx context.Context, repoFullName string, prNumber int, body string) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/issues/%d/comments", repoFullName, prNumber)
	payload, _ := json.Marshal(map[string]string{"body": body})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("post comment: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("post comment HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	return nil
}

type issueComment struct {
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	User      struct {
		Login string `json:"login"`
	} `json:"user"`
}

func (c *GitHubClient) listIssueComments(ctx context.Context, repoFullName string, prNumber int, query string) ([]issueComment, error) {
	nextURL := fmt.Sprintf("https://api.github.com/repos/%s/issues/%d/comments?%s", repoFullName, prNumber, query)
	var comments []issueComment

	for nextURL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, nextURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("list comments: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return nil, fmt.Errorf("list comments HTTP %d: %s", resp.StatusCode, string(body))
		}

		var page []issueComment
		if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("decode comments: %w", err)
		}

		comments = append(comments, page...)
		nextURL = nextPageURL(resp.Header.Get("Link"))
		_ = resp.Body.Close()
	}

	return comments, nil
}

// postComment checks for a recent duplicate, then posts. Returns (duplicate=true, nil)
// if the authenticated user already posted the same comment within the last minute.
func (c *GitHubClient) postComment(ctx context.Context, repoFullName string, prNumber int, body string) (duplicate bool, err error) {
	username := c.getUsername()
	if username == "" {
		return false, fmt.Errorf("authenticated GitHub username is not resolved")
	}

	comments, err := c.listIssueComments(ctx, repoFullName, prNumber, "sort=created&direction=desc&per_page=100")
	if err != nil {
		return false, err
	}

	for _, comment := range comments {
		if strings.EqualFold(comment.User.Login, username) && strings.TrimSpace(comment.Body) == body {
			if time.Since(comment.CreatedAt) < time.Minute {
				return true, nil
			}
		}
	}

	return false, c.postCommentRaw(ctx, repoFullName, prNumber, body)
}

// countMergeComments counts comments with the given body from the authenticated user.
// Returns the total count and the time of the most recent one.
func (c *GitHubClient) countMergeComments(ctx context.Context, repoFullName string, prNumber int, commentBody string) (count int, lastTime time.Time, err error) {
	username := c.getUsername()
	if username == "" {
		return 0, time.Time{}, fmt.Errorf("authenticated GitHub username is not resolved")
	}

	comments, err := c.listIssueComments(ctx, repoFullName, prNumber, "per_page=100")
	if err != nil {
		return 0, time.Time{}, err
	}

	for _, comment := range comments {
		if strings.EqualFold(comment.User.Login, username) && strings.TrimSpace(comment.Body) == commentBody {
			count++
			if comment.CreatedAt.After(lastTime) {
				lastTime = comment.CreatedAt
			}
		}
	}

	return count, lastTime, nil
}

type workflowRun struct {
	ID         int64     `json:"id"`
	RunAttempt int       `json:"run_attempt"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type failedWorkflowSummary struct {
	Count           int
	MaxAttempt      int
	LatestUpdatedAt time.Time
}

func (c *GitHubClient) failedWorkflowRunsSummary(ctx context.Context, repoFullName string, headSHA string) (failedWorkflowSummary, error) {
	runs, err := c.listWorkflowRuns(ctx, repoFullName, headSHA)
	if err != nil {
		return failedWorkflowSummary{}, err
	}

	var summary failedWorkflowSummary
	for _, run := range runs {
		if isFailedWorkflowConclusion(run.Conclusion) {
			summary.Count++
			if run.RunAttempt > summary.MaxAttempt {
				summary.MaxAttempt = run.RunAttempt
			}
			if run.UpdatedAt.After(summary.LatestUpdatedAt) {
				summary.LatestUpdatedAt = run.UpdatedAt
			}
		}
	}

	return summary, nil
}

func (c *GitHubClient) listWorkflowRuns(ctx context.Context, repoFullName string, headSHA string) ([]workflowRun, error) {
	nextURL := fmt.Sprintf("https://api.github.com/repos/%s/actions/runs?head_sha=%s&per_page=100", repoFullName, url.QueryEscape(headSHA))
	var allRuns []workflowRun

	for nextURL != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, nextURL, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)

		resp, err := c.httpClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("list runs: %w", err)
		}

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			return nil, fmt.Errorf("list runs HTTP %d: %s", resp.StatusCode, string(body))
		}

		var runs struct {
			WorkflowRuns []workflowRun `json:"workflow_runs"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&runs); err != nil {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("decode runs: %w", err)
		}

		allRuns = append(allRuns, runs.WorkflowRuns...)
		nextURL = nextPageURL(resp.Header.Get("Link"))
		_ = resp.Body.Close()
	}

	return allRuns, nil
}

func isFailedWorkflowConclusion(conclusion string) bool {
	switch conclusion {
	case "failure", "timed_out", "cancelled":
		return true
	default:
		return false
	}
}

func nextPageURL(linkHeader string) string {
	for part := range strings.SplitSeq(linkHeader, ",") {
		sections := strings.Split(part, ";")
		if len(sections) < 2 {
			continue
		}

		if strings.TrimSpace(sections[1]) != `rel="next"` {
			continue
		}

		u := strings.TrimSpace(sections[0])
		u = strings.TrimPrefix(u, "<")
		u = strings.TrimSuffix(u, ">")
		return u
	}
	return ""
}

func (c *GitHubClient) rerunFailedJobs(ctx context.Context, repoFullName string, headSHA string) error {
	runs, err := c.listWorkflowRuns(ctx, repoFullName, headSHA)
	if err != nil {
		return err
	}

	reran := 0
	for _, run := range runs {
		if isFailedWorkflowConclusion(run.Conclusion) {
			rerunURL := fmt.Sprintf("https://api.github.com/repos/%s/actions/runs/%d/rerun-failed-jobs", repoFullName, run.ID)

			rerunReq, err := http.NewRequestWithContext(ctx, http.MethodPost, rerunURL, nil)
			if err != nil {
				return err
			}

			rerunReq.Header.Set("Authorization", "Bearer "+c.token)

			rerunResp, err := c.httpClient.Do(rerunReq)
			if err != nil {
				return fmt.Errorf("rerun jobs: %w", err)
			}
			_ = rerunResp.Body.Close()

			if rerunResp.StatusCode != http.StatusCreated {
				return fmt.Errorf("rerun jobs HTTP %d for run %d", rerunResp.StatusCode, run.ID)
			}
			reran++
		}
	}

	if reran == 0 {
		return fmt.Errorf("no failed workflow runs found to re-run")
	}

	return nil
}

type pullRequestState struct {
	State  string `json:"state"`
	Merged bool   `json:"merged"`
}

func (c *GitHubClient) getPRState(ctx context.Context, repoFullName string, prNumber int) (pullRequestState, error) {
	url := fmt.Sprintf("https://api.github.com/repos/%s/pulls/%d", repoFullName, prNumber)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return pullRequestState{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return pullRequestState{}, fmt.Errorf("get PR: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return pullRequestState{}, fmt.Errorf("get PR HTTP %d: %s", resp.StatusCode, string(body))
	}

	var pr pullRequestState
	if err := json.NewDecoder(resp.Body).Decode(&pr); err != nil {
		return pullRequestState{}, fmt.Errorf("decode PR: %w", err)
	}

	return pr, nil
}

// deleteBranch deletes a branch from a repo. Used to clean up after merge.
func (c *GitHubClient) deleteBranch(ctx context.Context, repoFullName string, branch string) error {
	url := fmt.Sprintf("https://api.github.com/repos/%s/git/refs/heads/%s", repoFullName, branch)

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("delete branch: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete branch HTTP %d: %s", resp.StatusCode, string(body))
	}

	return nil
}
