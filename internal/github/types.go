package github

import "time"

// GraphQL response types — these mirror GitHub's PR search payload.

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
	BaseRefOID       string     `json:"baseRefOid"`
	BaseRef          *refNode   `json:"baseRef"`
	Mergeable        string     `json:"mergeable"`
	MergeStateStatus string     `json:"mergeStateStatus"`
	ReviewDecision   string     `json:"reviewDecision"`
	Author           authorNode `json:"author"`
	Repository       repoNode   `json:"repository"`
	HeadRef          refNode    `json:"headRef"`
	Commits          commitConn `json:"commits"`
	AllCommits       commitConn `json:"allCommits"`
	Reviews          reviewConn `json:"reviews"`
	ReviewThreads    threadConn `json:"reviewThreads"`
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
	Author            gitActor           `json:"author"`
	Committer         gitActor           `json:"committer"`
}

// gitActor is a commit author or committer. User is nil when the commit email
// is not linked to a GitHub account.
type gitActor struct {
	User *authorNode `json:"user"`
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

type threadConn struct {
	TotalCount int          `json:"totalCount"`
	Nodes      []threadNode `json:"nodes"`
}

type threadNode struct {
	IsResolved bool `json:"isResolved"`
}

type authorNode struct {
	Login string `json:"login"`
}

// WorkflowSummary aggregates the failed GitHub Actions runs for a head SHA.
type WorkflowSummary struct {
	Count           int
	MaxAttempt      int
	LatestUpdatedAt time.Time
	// Running counts runs that have not completed yet. A run with a failed job
	// has no conclusion until all its other jobs finish, and only then can its
	// failed jobs be re-run.
	Running int
	// AwaitingApproval counts runs from a fork that wait for a maintainer to
	// approve them. Their checks are missing from the rollup until then.
	AwaitingApproval int
}

// PRState is the merge state of a PR, read from the REST API.
type PRState struct {
	State  string `json:"state"`
	Merged bool   `json:"merged"`
}

type workflowRun struct {
	ID         int64     `json:"id"`
	RunAttempt int       `json:"run_attempt"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	UpdatedAt  time.Time `json:"updated_at"`
}

const prQuery = `query($query: String!, $after: String) {
  search(query: $query, type: ISSUE, first: 25, after: $after) {
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
        baseRefOid
        baseRef { target { oid } }
        mergeable
        mergeStateStatus
        reviewDecision
        author { login }
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
        reviewThreads(first: 100) {
          totalCount
          nodes { isResolved }
        }
        allCommits: commits(last: 100) {
          nodes {
            commit {
              author { user { login } }
              committer { user { login } }
            }
          }
        }
      }
    }
  }
}`
