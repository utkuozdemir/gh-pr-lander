package github

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/utkuozdemir/gh-pr-lander/internal/pr"
)

// Notes.
//
// A note is a review comment thread the user started on their own PR that
// nobody else replied to. It tells reviewers something and asks for nothing,
// so it must not count as an unresolved comment.
//
// Who wrote the comments of a thread cannot be asked for in the search: with
// 25 PRs times 100 threads the query costs 25 instead of 1. Asked per PR it
// costs 1. So it is only asked for a PR of the user's that has unresolved
// threads, and the answer is kept until the PR changes.

// threadAuthorsQuery looks at the first 20 comments of each thread. A longer
// thread is a conversation, not a note.
const threadAuthorsQuery = `query($owner: String!, $name: String!, $number: Int!) {
  repository(owner: $owner, name: $name) {
    pullRequest(number: $number) {
      reviewThreads(first: 100) {
        nodes {
          isResolved
          comments(first: 20) {
            totalCount
            nodes { author { login } }
          }
        }
      }
    }
  }
}`

type threadAuthorsResponse struct {
	Data struct {
		Repository struct {
			PullRequest struct {
				ReviewThreads struct {
					Nodes []struct {
						IsResolved bool `json:"isResolved"`
						Comments   struct {
							TotalCount int `json:"totalCount"`
							Nodes      []struct {
								Author authorNode `json:"author"`
							} `json:"nodes"`
						} `json:"comments"`
					} `json:"nodes"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
	Errors []graphQLError `json:"errors"`
}

// notesCache keeps the note count of a PR for as long as the PR looks the
// same: not updated since, and with the same thread counts.
type notesCache struct {
	mu      sync.Mutex
	entries map[string]notesEntry
}

type notesEntry struct {
	updatedAt       time.Time
	threads, solved int
	notes           int
}

// annotateNotes sets Notes on the user's own PRs that have unresolved threads.
// A lookup that fails leaves Notes at zero, so the threads count as unresolved.
func (c *Client) annotateNotes(ctx context.Context, prs []pr.PR) error {
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(annotateConcurrency)

	seen := make(map[string]bool, len(prs))

	for i := range prs {
		p := &prs[i]
		key := p.RepoShort()
		seen[key] = true

		if p.TakenOverFrom != "" || p.ReviewThreads == p.ResolvedReviewThreads {
			continue
		}

		if notes, ok := c.notes.get(key, *p); ok {
			p.Notes = notes

			continue
		}

		g.Go(func() error {
			notes, err := c.countNotes(gctx, *p)
			if err != nil {
				return nil //nolint:nilerr // best effort, the threads then count as unresolved
			}

			p.Notes = notes
			c.notes.put(key, *p, notes)

			return nil
		})
	}

	err := g.Wait()

	c.notes.keep(seen)

	return err
}

func (c *Client) countNotes(ctx context.Context, p pr.PR) (int, error) {
	variables := map[string]any{"owner": p.RepoOwner, "name": p.RepoName, "number": p.Number}

	var resp threadAuthorsResponse
	if err := c.graphql(ctx, threadAuthorsQuery, variables, &resp); err != nil {
		return 0, err
	}

	if len(resp.Errors) > 0 {
		return 0, fmt.Errorf("graphql errors for the threads of %s: %s", p.RepoShort(), resp.Errors[0].Message)
	}

	notes := 0

	for _, thread := range resp.Data.Repository.PullRequest.ReviewThreads.Nodes {
		comments := thread.Comments
		if thread.IsResolved || len(comments.Nodes) == 0 || comments.TotalCount > len(comments.Nodes) {
			continue
		}

		onlyTheUser := true

		for _, comment := range comments.Nodes {
			if !strings.EqualFold(comment.Author.Login, c.username) {
				onlyTheUser = false

				break
			}
		}

		if onlyTheUser {
			notes++
		}
	}

	return notes, nil
}

func (n *notesCache) get(key string, p pr.PR) (int, bool) {
	n.mu.Lock()
	defer n.mu.Unlock()

	e, ok := n.entries[key]
	if !ok || !e.updatedAt.Equal(p.UpdatedAt) || e.threads != p.ReviewThreads || e.solved != p.ResolvedReviewThreads {
		return 0, false
	}

	return e.notes, true
}

func (n *notesCache) put(key string, p pr.PR, notes int) {
	n.mu.Lock()
	defer n.mu.Unlock()

	if n.entries == nil {
		n.entries = make(map[string]notesEntry)
	}

	n.entries[key] = notesEntry{
		updatedAt: p.UpdatedAt, threads: p.ReviewThreads, solved: p.ResolvedReviewThreads, notes: notes,
	}
}

// keep drops the entries of PRs that are no longer listed.
func (n *notesCache) keep(listed map[string]bool) {
	n.mu.Lock()
	defer n.mu.Unlock()

	for key := range n.entries {
		if !listed[key] {
			delete(n.entries, key)
		}
	}
}
