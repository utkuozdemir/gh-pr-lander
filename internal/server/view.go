package server

import (
	"fmt"
	"strings"
	"time"

	"github.com/utkuozdemir/gh-pr-lander/internal/pr"
)

// The page is drawn from a copy of the state taken under the lock, so the
// template never touches the engine.

type flash struct {
	Text string
	Err  bool
}

type pageData struct {
	Version  string
	Flash    flash
	PRs      []prView
	Gone     []goneView
	Fetched  string
	FetchErr string
}

// mark is one piece of a PR's status: its text and the tone to show it in.
type mark struct {
	Text string
	Tone string
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
	Ago    string
}

func (s *Server) snapshot() pageData {
	s.mu.Lock()
	defer s.mu.Unlock()

	data := pageData{Fetched: "not yet", Version: s.state.Config.Version}

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

		if p.ReviewThreads > 0 {
			v.Comments = mark{fmt.Sprintf("%d/%d resolved", p.ResolvedReviewThreads, p.ReviewThreads), toneOk}
			if p.UnresolvedThreads() > 0 {
				v.Comments.Tone = toneWarn
			}
		}

		if v.AutoMerge {
			v.Waits = strings.Join(s.state.AutomergeWaitsFor(p), ", ")
		}

		switch {
		case v.MergeGaveUp:
			v.Badge = mark{"MERGE FAIL", toneFail}
		case v.RerunGaveUp:
			v.Badge = mark{"RERUN FAIL", toneFail}
		}

		data.PRs = append(data.PRs, v)
	}

	for _, mp := range s.state.SortedMerged() {
		g := goneView{
			Repo:   mp.PR.Repo,
			Number: mp.PR.Number,
			Title:  mp.PR.Title,
			URL:    mp.PR.URL,
			Badge:  mark{"CHECKING", toneNone},
			Ago:    ago(mp.GoneAt),
		}

		switch {
		case mp.Confirmed && mp.Merged:
			g.Badge = mark{"MERGED", toneOk}
		case mp.Confirmed:
			g.Badge = mark{"CLOSED", toneNone}
		}

		data.Gone = append(data.Gone, g)
	}

	return data
}

func ago(t time.Time) string {
	d := time.Since(t)

	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	default:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	}
}

func badge(status pr.OverallStatus) mark {
	switch status {
	case pr.StatusReady:
		return mark{"READY", toneOk}
	case pr.StatusStale:
		return mark{"STALE", toneWarn}
	case pr.StatusPending:
		return mark{"PENDING", toneInfo}
	case pr.StatusFailed:
		return mark{"FAILED", toneFail}
	case pr.StatusBlocked:
		return mark{"BLOCKED", toneFail}
	case pr.StatusDraft:
		return mark{"DRAFT", toneNone}
	default:
		return mark{}
	}
}

func baseMark(p pr.PR) mark {
	switch p.BaseStatus {
	case pr.BaseSynced:
		return mark{"synced", toneOk}
	case pr.BaseBehind:
		if p.BehindUnenforced {
			return mark{"behind (not enforced)", toneInfo}
		}

		return mark{"behind", toneWarn}
	case pr.BaseConflict:
		return mark{"conflict", toneFail}
	case pr.BaseUnknown:
		return mark{"checking", toneNone}
	default:
		return mark{}
	}
}

func ciMark(p pr.PR) mark {
	if p.AwaitingApprovalRuns > 0 {
		return mark{"workflow awaiting approval", toneWarn}
	}

	switch p.CIStatus {
	case pr.CIPassed:
		return mark{p.Checks.String(), toneOk}
	case pr.CIRunning:
		return mark{p.Checks.String(), toneInfo}
	case pr.CIFailed:
		return mark{p.Checks.String(), toneFail}
	case pr.CINone:
		return mark{"no checks", toneNone}
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
		return mark{"approved" + approvals, toneOk}
	case pr.ReviewChangesRequested:
		return mark{"changes requested" + approvals, toneFail}
	case pr.ReviewPending:
		return mark{"pending" + approvals, toneWarn}
	case pr.ReviewNone:
		// The repository asks for no review, but people approved anyway.
		if p.Approvals > 0 {
			return mark{"approved" + approvals, toneOk}
		}

		return mark{"none", toneNone}
	default:
		return mark{}
	}
}
