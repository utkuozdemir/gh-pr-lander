package server

import (
	"fmt"
	"strings"
	"time"

	"github.com/utkuozdemir/gh-pr-lander/internal/hint"
	"github.com/utkuozdemir/gh-pr-lander/internal/pr"
)

// The page is drawn from a copy of the state taken under the lock, so the
// template never touches the engine.

type flash struct {
	Text string
	Err  bool
}

type pageData struct {
	Hint     hints
	Version  string
	Flash    flash
	PRs      []prView
	Gone     []goneView
	Fetched  age
	FetchErr string
}

// hints are the hover texts of the page's controls, the same sentences the
// terminal app shows in its help view.
type hints struct {
	Automerge, Approvals, ApprovalsDefault, Approvals2, Approvals3 string
	Comments, CommentsAny, CommentsResolved, Autorerun             string
	Merge, MergeNotReady, Rerun, RerunNothing, Refresh, Open       string
	Updated, Version, MergeGaveUp, RerunGaveUp                     string
}

var pageHints = hints{
	Automerge:        hint.Automerge,
	Approvals:        hint.Approvals,
	ApprovalsDefault: hint.ApprovalsDefault,
	Approvals2:       hint.ApprovalsAtLeast(2),
	Approvals3:       hint.ApprovalsAtLeast(3),
	Comments:         hint.Comments,
	CommentsAny:      hint.CommentsAny,
	CommentsResolved: hint.CommentsResolved,
	Autorerun:        hint.Autorerun,
	Merge:            hint.Merge,
	MergeNotReady:    hint.MergeNotReady,
	Rerun:            hint.Rerun,
	RerunNothing:     hint.RerunNothing,
	Refresh:          hint.Refresh,
	Open:             hint.Open,
	Updated:          hint.Updated,
	Version:          hint.Version,
	MergeGaveUp:      hint.MergeGaveUp,
	RerunGaveUp:      hint.RerunGaveUp,
}

// mark is one piece of a PR's status: its text, the tone to show it in, and
// the explanation shown on hover.
type mark struct {
	Text string
	Tone string
	Hint string
}

// The tones are the CSS classes of the page.
const (
	toneOk   = "ok"
	toneWarn = "warn"
	toneFail = "fail"
	toneInfo = "info"
	toneNone = "none"
)

type prView struct {
	Key    string
	Repo   string
	Number int
	Title  string
	URL    string
	SHA    string

	Badge  mark
	Base   mark
	CI     mark
	Review mark
	// Reruns is how often the checks of the head commit were re-run.
	Reruns mark
	// Notes is the owner's notes on the PR, which are left out of Comments.
	Notes mark
	// Comments is the resolved and total review comment threads, empty when
	// the PR has none.
	Comments mark

	AutoMerge bool
	// Approvals and ResolvedOnly are the automerge conditions, and Waits is
	// what they still ask for.
	Approvals    int // 1 means no condition: the repository's own rule
	ResolvedOnly bool
	Waits        string

	AutoRerun   bool
	MergeGaveUp bool
	RerunGaveUp bool
	CanMerge    bool
	CanRerun    bool
}

type goneView struct {
	Repo   string
	Number int
	Title  string
	URL    string
	Badge  mark
	Ago    age
}

func (s *Server) snapshot() pageData {
	s.mu.Lock()
	defer s.mu.Unlock()

	data := pageData{Fetched: age{Text: "not yet", Seconds: -1}, Version: s.state.Config.Version, Hint: pageHints}

	if !s.lastFetch.IsZero() {
		data.Fetched = ago(s.lastFetch)
	}

	if s.fetchErr != nil {
		data.FetchErr = s.fetchErr.Error()
	}

	for _, p := range s.state.PRs {
		key := p.RepoShort()

		v := prView{
			Key:          key,
			Repo:         p.Repo,
			Number:       p.Number,
			Title:        p.Title,
			URL:          p.URL,
			SHA:          p.HeadSHA,
			Badge:        badge(p.OverallStatus),
			Base:         baseMark(p),
			CI:           ciMark(p),
			Review:       reviewMark(p),
			AutoMerge:    s.state.AutoMerge[key],
			Approvals:    max(s.state.MinApprovals[key], 1),
			ResolvedOnly: s.state.ResolvedOnly[key],
			AutoRerun:    s.state.AutoRerun[key],
			MergeGaveUp:  s.state.IsMergeFailed(key),
			RerunGaveUp:  s.state.IsRerunFailed(key),
			CanMerge:     p.OverallStatus == pr.StatusReady,
			CanRerun:     p.CIStatus == pr.CIFailed,
		}

		if p.CommentThreads() > 0 {
			v.Comments = mark{
				fmt.Sprintf("%d/%d resolved", p.ResolvedReviewThreads, p.CommentThreads()), toneOk, hint.CommentCount,
			}
			if p.UnresolvedThreads() > 0 {
				v.Comments.Tone = toneWarn
			}
		}

		if reruns := s.state.RerunsLabel(p); reruns != "" {
			v.Reruns = mark{reruns, toneNone, hint.Reruns}
		}

		if p.Notes > 0 {
			v.Notes = mark{p.NotesLabel(), toneNone, hint.Note}
		}

		if v.AutoMerge {
			v.Waits = strings.Join(s.state.AutomergeWaitsFor(p), ", ")
		}

		switch {
		case v.MergeGaveUp:
			v.Badge = mark{"MERGE FAIL", toneFail, hint.MergeGaveUp}
		case v.RerunGaveUp:
			v.Badge = mark{"RERUN FAIL", toneFail, hint.RerunGaveUp}
		}

		data.PRs = append(data.PRs, v)
	}

	for _, mp := range s.state.SortedMerged() {
		g := goneView{
			Repo:   mp.PR.Repo,
			Number: mp.PR.Number,
			Title:  mp.PR.Title,
			URL:    mp.PR.URL,
			Badge:  mark{"CHECKING", toneNone, hint.Checking},
			Ago:    ago(mp.GoneAt),
		}

		switch {
		case mp.Confirmed && mp.Merged:
			g.Badge = mark{"MERGED", toneOk, hint.Merged}
		case mp.Confirmed:
			g.Badge = mark{"CLOSED", toneNone, hint.Closed}
		}

		data.Gone = append(data.Gone, g)
	}

	return data
}

// age is how long ago something happened: in words for the page as it is
// served, and in seconds for the page's script, which keeps the words counting
// between refreshes. The script counts from the moment it received the
// seconds, so the clocks of the server and the browser never have to agree.
type age struct {
	Text    string
	Seconds int // negative when it has not happened yet
}

// ago words an age the same way the script in page.html does.
func ago(t time.Time) age {
	seconds := max(int(time.Since(t).Seconds()), 0)

	var text string

	switch {
	case seconds < 60:
		text = fmt.Sprintf("%ds ago", seconds)
	case seconds < 3600:
		text = fmt.Sprintf("%dm %ds ago", seconds/60, seconds%60)
	default:
		text = fmt.Sprintf("%dh %dm ago", seconds/3600, seconds%3600/60)
	}

	return age{Text: text, Seconds: seconds}
}

func badge(status pr.OverallStatus) mark {
	switch status {
	case pr.StatusReady:
		return mark{"READY", toneOk, hint.Ready}
	case pr.StatusStale:
		return mark{"STALE", toneWarn, hint.Stale}
	case pr.StatusPending:
		return mark{"PENDING", toneInfo, hint.Pending}
	case pr.StatusFailed:
		return mark{"FAILED", toneFail, hint.Failed}
	case pr.StatusBlocked:
		return mark{"BLOCKED", toneFail, hint.Blocked}
	case pr.StatusDraft:
		return mark{"DRAFT", toneNone, hint.Draft}
	default:
		return mark{}
	}
}

func baseMark(p pr.PR) mark {
	switch p.BaseStatus {
	case pr.BaseSynced:
		return mark{"synced", toneOk, hint.BaseSynced}
	case pr.BaseBehind:
		if p.BehindUnenforced {
			return mark{"behind (not enforced)", toneInfo, hint.BaseUnenforced}
		}

		return mark{"behind", toneWarn, hint.BaseBehind}
	case pr.BaseConflict:
		return mark{"conflict", toneFail, hint.BaseConflict}
	case pr.BaseUnknown:
		return mark{"checking", toneNone, hint.BaseUnknown}
	default:
		return mark{}
	}
}

// checksHint explains passed checks, including why a PR counts as passed while
// some are still running.
func checksHint(p pr.PR) string {
	if p.Checks.Optional > 0 {
		return hint.ChecksOptional
	}

	return hint.Checks
}

func ciMark(p pr.PR) mark {
	if p.AwaitingApprovalRuns > 0 {
		return mark{"workflow awaiting approval", toneWarn, hint.ChecksUnapproved}
	}

	switch p.CIStatus {
	case pr.CIPassed:
		return mark{p.Checks.String(), toneOk, checksHint(p)}
	case pr.CIRunning:
		return mark{p.Checks.String(), toneInfo, hint.Checks}
	case pr.CIFailed:
		return mark{p.Checks.String(), toneFail, hint.Checks}
	case pr.CINone:
		return mark{"no checks", toneNone, hint.Checks}
	default:
		return mark{}
	}
}

// reviewMark is the review state, with the number of approvals behind it when
// there are any.
func reviewMark(p pr.PR) mark {
	approvals := ""
	if p.Approvals > 0 {
		approvals = fmt.Sprintf(" (%d)", p.Approvals)
	}

	switch p.ReviewStatus {
	case pr.ReviewApproved:
		return mark{"approved" + approvals, toneOk, hint.Review}
	case pr.ReviewChangesRequested:
		return mark{"changes requested" + approvals, toneFail, hint.Review}
	case pr.ReviewPending:
		return mark{"pending" + approvals, toneWarn, hint.Review}
	case pr.ReviewNone:
		// The repository asks for no review, but people approved anyway.
		if p.Approvals > 0 {
			return mark{"approved" + approvals, toneOk, hint.Review}
		}

		return mark{"none", toneNone, hint.Review}
	default:
		return mark{}
	}
}
