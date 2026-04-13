package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

func (c *GitHubClient) getUsername(ctx context.Context) (string, error) {
	if c.username != "" {
		return c.username, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/user", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()

	var user struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return "", err
	}

	c.username = user.Login
	return c.username, nil
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
	Nodes []prNode `json:"nodes"`
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
	OID                string              `json:"oid"`
	StatusCheckRollup  *statusCheckRollup  `json:"statusCheckRollup"`
}

type statusCheckRollup struct {
	State    string       `json:"state"`
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

const prQuery = `query($query: String!) {
  search(query: $query, type: ISSUE, first: 50) {
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
	orgFilter := make([]string, len(orgs))
	for i, org := range orgs {
		orgFilter[i] = "org:" + org
	}
	searchQuery := fmt.Sprintf("is:pr is:open author:@me %s", strings.Join(orgFilter, " "))

	variables := map[string]any{"query": searchQuery}

	var resp graphQLResponse
	if err := c.graphql(ctx, prQuery, variables, &resp); err != nil {
		return nil, err
	}

	if len(resp.Errors) > 0 {
		msgs := make([]string, len(resp.Errors))
		for i, e := range resp.Errors {
			msgs[i] = e.Message
		}
		return nil, fmt.Errorf("graphql errors: %s", strings.Join(msgs, "; "))
	}

	prs := make([]PR, 0, len(resp.Data.Search.Nodes))
	for _, node := range resp.Data.Search.Nodes {
		prs = append(prs, prFromNode(node))
	}

	sortPRs(prs)

	return prs, nil
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

// postComment checks for a recent duplicate, then posts. Returns (duplicate=true, nil)
// if the authenticated user already posted the same comment within the last minute.
func (c *GitHubClient) postComment(ctx context.Context, repoFullName string, prNumber int, body string) (duplicate bool, err error) {
	username, err := c.getUsername(ctx)
	if err != nil {
		return false, fmt.Errorf("get username: %w", err)
	}

	listURL := fmt.Sprintf("https://api.github.com/repos/%s/issues/%d/comments?sort=created&direction=desc&per_page=5", repoFullName, prNumber)
	listReq, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
	if err != nil {
		return false, err
	}
	listReq.Header.Set("Authorization", "Bearer "+c.token)

	listResp, err := c.httpClient.Do(listReq)
	if err != nil {
		return false, fmt.Errorf("list comments: %w", err)
	}
	defer func() { _ = listResp.Body.Close() }()

	var comments []struct {
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := json.NewDecoder(listResp.Body).Decode(&comments); err != nil {
		return false, fmt.Errorf("decode comments: %w", err)
	}

	for _, comment := range comments {
		if strings.EqualFold(comment.User.Login, username) && strings.TrimSpace(comment.Body) == body {
			created, parseErr := time.Parse(time.RFC3339, comment.CreatedAt)
			if parseErr == nil && time.Since(created) < time.Minute {
				return true, nil
			}
		}
	}

	return false, c.postCommentRaw(ctx, repoFullName, prNumber, body)
}

// countMergeComments counts "/m" comments from the authenticated user on a PR.
// Returns the total count and the time of the most recent one.
func (c *GitHubClient) countMergeComments(ctx context.Context, repoFullName string, prNumber int, commentBody string) (count int, lastTime time.Time, err error) {
	username, err := c.getUsername(ctx)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("get username: %w", err)
	}

	url := fmt.Sprintf("https://api.github.com/repos/%s/issues/%d/comments?per_page=100", repoFullName, prNumber)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, time.Time{}, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("list comments: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var comments []struct {
		Body      string `json:"body"`
		CreatedAt string `json:"created_at"`
		User      struct {
			Login string `json:"login"`
		} `json:"user"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&comments); err != nil {
		return 0, time.Time{}, fmt.Errorf("decode comments: %w", err)
	}

	for _, comment := range comments {
		if strings.EqualFold(comment.User.Login, username) && strings.TrimSpace(comment.Body) == commentBody {
			count++
			created, parseErr := time.Parse(time.RFC3339, comment.CreatedAt)
			if parseErr == nil && created.After(lastTime) {
				lastTime = created
			}
		}
	}

	return count, lastTime, nil
}

func (c *GitHubClient) rerunFailedJobs(ctx context.Context, repoFullName string, headSHA string) error {
	// First, find workflow runs for this commit
	url := fmt.Sprintf("https://api.github.com/repos/%s/actions/runs?head_sha=%s", repoFullName, headSHA)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("list runs: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var runs struct {
		WorkflowRuns []struct {
			ID         int64  `json:"id"`
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"workflow_runs"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&runs); err != nil {
		return fmt.Errorf("decode runs: %w", err)
	}

	reran := 0
	for _, run := range runs.WorkflowRuns {
		if run.Conclusion == "failure" || run.Conclusion == "timed_out" || run.Conclusion == "cancelled" {
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
