// Package github is the only place that talks to the GitHub API. It exposes a
// concrete *Client plus narrow consumer-defined interfaces are declared by the
// callers (e.g. internal/tui). Everything returns domain types from
// internal/pr, never raw API shapes.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/utkuozdemir/sidero-rebaser/internal/pr"
)

// annotateConcurrency bounds how many Actions-API calls we make in parallel
// when enriching failed PRs, so a user with many failing PRs does not hammer
// the API.
const annotateConcurrency = 6

// Client is an authenticated GitHub API client.
type Client struct {
	token      string
	httpClient *http.Client
	username   string // cached authenticated user login
}

// New constructs a Client with the given token.
func New(token string) *Client {
	return &Client{
		token:      token,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
}

// ResolveUsername fetches and caches the authenticated user's login. Call once
// at startup to avoid races from concurrent reads.
func (c *Client) ResolveUsername(ctx context.Context) error {
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

// Username returns the cached authenticated user login.
func (c *Client) Username() string {
	return c.username
}

func (c *Client) graphql(ctx context.Context, query string, variables map[string]any, result any) error {
	body := map[string]any{
		"query":     query,
		"variables": variables,
	}

	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal graphql request: %w", err)
	}

	req, err := http.NewRequestWithContext(
		ctx,
		http.MethodPost,
		"https://api.github.com/graphql",
		bytes.NewReader(data),
	)
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

// FetchPRs returns the authenticated user's open PRs across the given orgs,
// including PRs opened by others that the user has authored or committed to.
// Orgs are queried in parallel; a PR that appears under multiple orgs is
// de-duplicated by its repo#number key.
func (c *Client) FetchPRs(ctx context.Context, orgs []string) ([]pr.PR, error) {
	results := make([][]pr.PR, len(orgs))

	g, gctx := errgroup.WithContext(ctx)
	for i, org := range orgs {
		g.Go(func() error {
			orgPRs, err := c.fetchPRsForOrg(gctx, org)
			if err != nil {
				return err
			}

			results[i] = orgPRs

			return nil
		})
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}

	seen := make(map[string]pr.PR)

	for _, orgPRs := range results {
		for _, p := range orgPRs {
			seen[p.RepoShort()] = p
		}
	}

	prs := make([]pr.PR, 0, len(seen))
	for _, p := range seen {
		prs = append(prs, p)
	}

	if err := c.annotateFailedWorkflowRuns(ctx, prs); err != nil {
		return nil, err
	}

	pr.Sort(prs)

	return prs, nil
}

func (c *Client) fetchPRsForOrg(ctx context.Context, org string) ([]pr.PR, error) {
	// involves:@me also matches PRs the user only commented on or was mentioned
	// in. It is just a cheap candidate set, filtered down by touchedBy below.
	searchQuery := "is:pr is:open involves:@me org:" + org

	var (
		prs   []pr.PR
		after any
	)

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
			if node.Repository.IsArchived || !touchedBy(node, c.username) {
				continue
			}

			p := prFromNode(node)
			if !strings.EqualFold(node.Author.Login, c.username) {
				p.TakenOverFrom = node.Author.Login
			}

			prs = append(prs, p)
		}

		page := resp.Data.Search.PageInfo
		if !page.HasNextPage || page.EndCursor == "" {
			break
		}

		after = page.EndCursor
	}

	return prs, nil
}

// annotateFailedWorkflowRuns enriches failed PRs with Actions-API data
// (failed run count, max attempt, latest update). Runs are fetched in parallel
// with bounded concurrency. Each goroutine writes only its own slice index, so
// no locking is needed. Individual fetch errors are tolerated (best effort).
func (c *Client) annotateFailedWorkflowRuns(ctx context.Context, prs []pr.PR) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(annotateConcurrency)

	for i := range prs {
		if prs[i].CIStatus != pr.CIFailed || prs[i].HeadSHA == "" {
			continue
		}

		g.Go(func() error {
			summary, err := c.FailedWorkflowRunsSummary(gctx, prs[i].Repo, prs[i].HeadSHA)
			if err != nil {
				// Best effort: leave this PR un-annotated rather than failing
				// the whole fetch.
				return nil //nolint:nilerr // enrichment is optional, never fail the fetch over it
			}

			prs[i].FailedWorkflowRunsKnown = true
			prs[i].FailedWorkflowRuns = summary.Count
			prs[i].MaxFailedRunAttempt = summary.MaxAttempt
			prs[i].LatestFailedRunUpdate = summary.LatestUpdatedAt
			prs[i].RunningWorkflowRuns = summary.Running

			return nil
		})
	}

	return g.Wait()
}

// PostCommentRaw posts a comment without any duplicate checking.
func (c *Client) PostCommentRaw(ctx context.Context, repoFullName string, prNumber int, body string) error {
	target := fmt.Sprintf("https://api.github.com/repos/%s/issues/%d/comments", repoFullName, prNumber)

	payload, err := json.Marshal(map[string]string{"body": body})
	if err != nil {
		return fmt.Errorf("marshal comment: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(payload))
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

func (c *Client) listIssueComments(
	ctx context.Context,
	repoFullName string,
	prNumber int,
	query string,
) ([]issueComment, error) {
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

// PostComment checks for a recent duplicate, then posts. Returns
// (duplicate=true, nil) if the authenticated user already posted the same
// comment within the last minute.
func (c *Client) PostComment(
	ctx context.Context,
	repoFullName string,
	prNumber int,
	body string,
) (duplicate bool, err error) {
	if c.username == "" {
		return false, errors.New("authenticated GitHub username is not resolved")
	}

	comments, err := c.listIssueComments(ctx, repoFullName, prNumber, "sort=created&direction=desc&per_page=100")
	if err != nil {
		return false, err
	}

	for _, comment := range comments {
		if strings.EqualFold(comment.User.Login, c.username) && strings.TrimSpace(comment.Body) == body {
			if time.Since(comment.CreatedAt) < time.Minute {
				return true, nil
			}
		}
	}

	return false, c.PostCommentRaw(ctx, repoFullName, prNumber, body)
}

// CountMergeComments counts comments with the given body from the
// authenticated user. Returns the total count and the time of the most recent.
func (c *Client) CountMergeComments(
	ctx context.Context,
	repoFullName string,
	prNumber int,
	commentBody string,
) (count int, lastTime time.Time, err error) {
	if c.username == "" {
		return 0, time.Time{}, errors.New("authenticated GitHub username is not resolved")
	}

	comments, err := c.listIssueComments(ctx, repoFullName, prNumber, "per_page=100")
	if err != nil {
		return 0, time.Time{}, err
	}

	for _, comment := range comments {
		if strings.EqualFold(comment.User.Login, c.username) && strings.TrimSpace(comment.Body) == commentBody {
			count++

			if comment.CreatedAt.After(lastTime) {
				lastTime = comment.CreatedAt
			}
		}
	}

	return count, lastTime, nil
}

// FailedWorkflowRunsSummary aggregates the failed Actions runs for a head SHA.
func (c *Client) FailedWorkflowRunsSummary(
	ctx context.Context,
	repoFullName string,
	headSHA string,
) (WorkflowSummary, error) {
	runs, err := c.listWorkflowRuns(ctx, repoFullName, headSHA)
	if err != nil {
		return WorkflowSummary{}, err
	}

	var summary WorkflowSummary

	for _, run := range runs {
		if run.Status != "completed" {
			summary.Running++
		}

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

func (c *Client) listWorkflowRuns(ctx context.Context, repoFullName string, headSHA string) ([]workflowRun, error) {
	nextURL := fmt.Sprintf(
		"https://api.github.com/repos/%s/actions/runs?head_sha=%s&per_page=100",
		repoFullName,
		url.QueryEscape(headSHA),
	)

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

// RerunFailedJobs re-runs the failed jobs of every failed workflow run on the
// given head SHA. Returns an error if there were none to re-run.
func (c *Client) RerunFailedJobs(ctx context.Context, repoFullName string, headSHA string) error {
	runs, err := c.listWorkflowRuns(ctx, repoFullName, headSHA)
	if err != nil {
		return err
	}

	reran := 0

	for _, run := range runs {
		if !isFailedWorkflowConclusion(run.Conclusion) {
			continue
		}

		rerunURL := fmt.Sprintf(
			"https://api.github.com/repos/%s/actions/runs/%d/rerun-failed-jobs",
			repoFullName,
			run.ID,
		)

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

	if reran == 0 {
		return errors.New("no failed workflow runs found to re-run")
	}

	return nil
}

// GetPRState reads the open/closed/merged state of a PR.
func (c *Client) GetPRState(ctx context.Context, repoFullName string, prNumber int) (PRState, error) {
	target := fmt.Sprintf("https://api.github.com/repos/%s/pulls/%d", repoFullName, prNumber)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return PRState{}, err
	}

	req.Header.Set("Authorization", "Bearer "+c.token)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return PRState{}, fmt.Errorf("get PR: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)

		return PRState{}, fmt.Errorf("get PR HTTP %d: %s", resp.StatusCode, string(body))
	}

	var state PRState
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return PRState{}, fmt.Errorf("decode PR: %w", err)
	}

	return state, nil
}

// DeleteBranch deletes a branch from a repo. Used to clean up after merge.
func (c *Client) DeleteBranch(ctx context.Context, repoFullName string, branch string) error {
	target := fmt.Sprintf("https://api.github.com/repos/%s/git/refs/heads/%s", repoFullName, branch)

	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, target, nil)
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
