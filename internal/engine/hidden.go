package engine

import (
	"github.com/utkuozdemir/gh-pr-lander/internal/notify"
	"github.com/utkuozdemir/gh-pr-lander/internal/pr"
)

// A hidden PR is one the owner chose not to look at, such as an old one that
// is blocked on something. Hiding changes what is shown and what notifies,
// and nothing else: the PR is still fetched, and its automerge, autorerun and
// place in the autorebase queue go on. It stays hidden until the owner
// un-hides it, and is forgotten when the PR is confirmed gone (cleanupAuto).

// ToggleHidden hides the PR or shows it again, and reports which.
func (s *State) ToggleHidden(p pr.PR) (bool, error) {
	key := p.RepoShort()

	if s.Hidden[key] {
		delete(s.Hidden, key)
	} else {
		s.Hidden[key] = true
	}

	return s.Hidden[key], s.SaveDB()
}

// Shown splits the PRs into the ones in the list and the hidden ones, each in
// the order of the fetch.
func (s *State) Shown() (listed, hidden []pr.PR) {
	for _, p := range s.PRs {
		if s.Hidden[p.RepoShort()] {
			hidden = append(hidden, p)
		} else {
			listed = append(listed, p)
		}
	}

	return listed, hidden
}

// Notify sends a desktop notification about a PR. A hidden PR is quiet, except
// for the news that it was merged, which ends the reason to hide it.
func (s *State) Notify(key string, event notify.Event, body string) {
	if s.Hidden[key] && event != notify.Merged {
		return
	}

	notify.Send(event, body)
}
