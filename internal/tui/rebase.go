package tui

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/utkuozdemir/sidero-rebaser/internal/git"
	"github.com/utkuozdemir/sidero-rebaser/internal/notify"
	"github.com/utkuozdemir/sidero-rebaser/internal/pr"
)

// Auto-rebase queue.
//
// With fast-forward merges only one PR per repo and base branch can usefully be
// in flight: merging one makes every other PR on that branch behind again, so
// rebasing them in parallel only burns CI runners. Each repo and base branch is
// therefore a queue, ordered by when auto-rebase was turned on (reorderable).
// The first eligible PR that is not behind is the active one and is left alone.
// When none is, only the first eligible PR is rebased. Everything is derived
// from the current fetch, nothing but the order is stored.
//
// A failed rebase is not retried: it is almost always a missed YubiKey touch,
// so the owner is away and a retry would fail the same way. The PR keeps its
// queue paused until the owner retries with b, or its head commit changes.

// autoRebaseState is the in-memory bookkeeping of one PR's last auto-rebase.
// It applies only while the PR's head is still sha, so a new push voids it.
type autoRebaseState struct {
	sha    string // head SHA the rebase started from
	reason string // why it failed, empty when it succeeded
}

// queueSlot is a PR's place in its auto-rebase queue.
type queueSlot struct {
	pos, total int
	active     bool
}

type autoRebaseResultMsg struct {
	pr     pr.PR
	output string
	err    error
}

func queueKey(p pr.PR) string {
	return p.Repo + "\x00" + p.BaseBranch
}

// rebaseQueues returns the PRs with auto-rebase on, grouped by repo and base
// branch, each group in queue order.
func (m model) rebaseQueues() map[string][]pr.PR {
	queues := make(map[string][]pr.PR)

	for _, p := range m.prs {
		if _, ok := m.autoRebase[p.RepoShort()]; ok {
			queues[queueKey(p)] = append(queues[queueKey(p)], p)
		}
	}

	for _, q := range queues {
		slices.SortFunc(q, func(a, b pr.PR) int {
			return m.autoRebase[a.RepoShort()] - m.autoRebase[b.RepoShort()]
		})
	}

	return queues
}

// rebaseEligible reports whether a PR could merge once it is up to date, so it
// is worth holding the queue for. Unapproved, failing or conflicting PRs are
// skipped, since rebasing them only burns CI. A failure autorerun is about to
// re-run does not count, since it is usually a flake. A PR whose rebase failed
// stays eligible, so it keeps the queue paused instead of handing it on while
// the owner is away.
func (m model) rebaseEligible(p pr.PR) bool {
	return !p.IsDraft &&
		p.BaseStatus != pr.BaseConflict &&
		(p.CIStatus != pr.CIFailed || m.autorerunWillHandle(p)) &&
		p.ReviewStatus != pr.ReviewPending &&
		p.ReviewStatus != pr.ReviewChangesRequested
}

// inFlight reports whether the PR needs no rebase now: it is up to date, being
// rebased, or its last auto-rebase started from its current head. That last
// case covers both a rebase GitHub has not caught up with yet and a failed one
// waiting for the owner.
func (m model) inFlight(p pr.PR) bool {
	key := p.RepoShort()
	sha := m.rebaseState[key].sha

	return p.BaseStatus != pr.BaseBehind || m.rebasing == key || (sha != "" && sha == p.HeadSHA)
}

// rebaseFailure returns why the last auto-rebase of the PR's current head
// failed, if it did.
func (m model) rebaseFailure(p pr.PR) (string, bool) {
	st := m.rebaseState[p.RepoShort()]
	if st.reason == "" || st.sha != p.HeadSHA {
		return "", false
	}

	return st.reason, true
}

// failureReason turns the rebase script output into a short reason for the
// row. The matched strings are what git prints for each failure.
func failureReason(output string) string {
	switch {
	case strings.Contains(output, "failed to write commit object"):
		return "signing failed, YubiKey touch missed?"
	case strings.Contains(output, "CONFLICT"):
		return "conflicts"
	case strings.Contains(output, "failed to push some refs"):
		return "push rejected"
	default:
		return "see the log"
	}
}

// activeInQueue returns the PR that holds the queue: the first eligible one
// in flight, else the first eligible one.
func (m model) activeInQueue(queue []pr.PR) (pr.PR, bool) {
	var first *pr.PR

	for i := range queue {
		if !m.rebaseEligible(queue[i]) {
			continue
		}

		if m.inFlight(queue[i]) {
			return queue[i], true
		}

		if first == nil {
			first = &queue[i]
		}
	}

	if first == nil {
		return pr.PR{}, false
	}

	return *first, true
}

func (m model) queueSlots() map[string]queueSlot {
	slots := make(map[string]queueSlot)

	for _, queue := range m.rebaseQueues() {
		active, hasActive := m.activeInQueue(queue)

		for i, p := range queue {
			slots[p.RepoShort()] = queueSlot{
				pos:    i + 1,
				total:  len(queue),
				active: hasActive && p.RepoShort() == active.RepoShort(),
			}
		}
	}

	return slots
}

// nextAutoRebase picks the one PR to rebase now, if any. Only one rebase runs
// at a time across all queues, since each needs a YubiKey touch and queues on
// different base branches of a repo share one clone.
func (m model) nextAutoRebase() (pr.PR, bool) {
	if m.rebasing != "" {
		return pr.PR{}, false
	}

	queues := m.rebaseQueues()

	for _, k := range slices.Sorted(maps.Keys(queues)) {
		if p, ok := m.activeInQueue(queues[k]); ok && !m.inFlight(p) {
			return p, true
		}
	}

	return pr.PR{}, false
}

func (m *model) autoRebaseCmd() tea.Cmd {
	p, ok := m.nextAutoRebase()
	if !ok {
		return nil
	}

	m.rebasing = p.RepoShort()
	slog.Info("autorebase started", "pr", m.rebasing)

	return runAutoRebaseCmd(m.config.DataDir, p)
}

// runAutoRebaseCmd runs the same script as a manual rebase, but in the
// background: stdin is /dev/null, so its "press Enter" prompts return at once,
// and signing asks for the YubiKey touch through the gpg program's own prompt.
func runAutoRebaseCmd(dataDir string, p pr.PR) tea.Cmd {
	return func() tea.Msg {
		script, err := git.BuildRebaseScript(dataDir, p, 0)
		if err != nil {
			return autoRebaseResultMsg{pr: p, err: err}
		}

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()

		cmd := exec.CommandContext(ctx, "bash", "-c", script)
		// The timeout kills only bash. Without this, a hung git or ssh child
		// keeps the output pipe open and the rebase never reports back.
		cmd.WaitDelay = 10 * time.Second

		out, err := cmd.CombinedOutput()

		return autoRebaseResultMsg{pr: p, output: string(out), err: err}
	}
}

func (m model) handleAutoRebaseResult(msg autoRebaseResultMsg) (tea.Model, tea.Cmd) {
	key := msg.pr.RepoShort()
	m.rebasing = ""

	if msg.err == nil {
		slog.Info("autorebase completed", "pr", key)

		m.rebaseState[key] = autoRebaseState{sha: msg.pr.HeadSHA}
		m.status = successStyle.Render("⚡ Autorebase: rebased " + key)
		m.triggerRefresh()

		return m, tea.Batch(clearStatusCmd(), delayedRefreshCmd(15*time.Second), delayedRefreshCmd(30*time.Second))
	}

	reason := failureReason(msg.output)
	slog.Error("autorebase failed", "pr", key, "reason", reason, "error", msg.err, "output", msg.output)

	m.rebaseState[key] = autoRebaseState{sha: msg.pr.HeadSHA, reason: reason}
	m.status = errorStyle.Render(fmt.Sprintf("⚡ Autorebase of %s failed: %s. Press b on it to retry", key, reason))
	notify.Send(notify.AutorebaseNeedsYou, key+": "+reason)

	// Other queues can go on, this one stays paused.
	next := m.autoRebaseCmd()

	return m, tea.Batch(clearStatusCmd(), next)
}

func (m model) toggleAutoRebase() (tea.Model, tea.Cmd) {
	item, ok := m.selectedItem()
	if !ok || item.merged {
		return m, nil
	}

	key := item.pr.RepoShort()
	_, on := m.autoRebase[key]
	_, failed := m.rebaseFailure(item.pr)

	switch {
	case on && failed:
		delete(m.rebaseState, key)
		slog.Info("autorebase retry", "pr", key)
		m.status = indicatorAuto.Render("⚡ retrying autorebase of " + key)
	case on:
		delete(m.autoRebase, key)
		delete(m.rebaseState, key)
		slog.Info("autorebase disabled", "pr", key)
		m.status = indicatorAuto.Render("⚡ autorebase disabled for " + key)
	default:
		last := 0
		for _, order := range m.autoRebase {
			last = max(last, order)
		}

		m.autoRebase[key] = last + 1
		slog.Info("autorebase enabled", "pr", key)
		m.status = indicatorAuto.Render("⚡ autorebase enabled for " + key)
	}

	if err := m.saveDB(); err != nil {
		slog.Error("failed to save db", "error", err)
		m.status = errorStyle.Render("✗ Failed to save db.yaml: " + err.Error())
	}

	// Turning a PR off can hand its queue to the next one, so check either way.
	next := m.autoRebaseCmd()

	return m, tea.Batch(clearStatusCmd(), next)
}

// moveInQueue swaps the selected PR with its neighbor in its auto-rebase queue.
// It never interrupts the active PR: the new order applies to who goes next.
func (m model) moveInQueue(delta int) (tea.Model, tea.Cmd) {
	item, ok := m.selectedItem()
	if !ok || item.merged {
		return m, nil
	}

	key := item.pr.RepoShort()
	if _, on := m.autoRebase[key]; !on {
		m.status = indicatorWarn.Render("⚠ Autorebase is off for " + key + ", press b to queue it")

		return m, clearStatusCmd()
	}

	queue := m.rebaseQueues()[queueKey(item.pr)]
	i := slices.IndexFunc(queue, func(p pr.PR) bool { return p.RepoShort() == key })

	j := i + delta
	if j < 0 || j >= len(queue) {
		return m, nil
	}

	other := queue[j].RepoShort()
	m.autoRebase[key], m.autoRebase[other] = m.autoRebase[other], m.autoRebase[key]

	if err := m.saveDB(); err != nil {
		slog.Error("failed to save db", "error", err)
		m.status = errorStyle.Render("✗ Failed to save db.yaml: " + err.Error())

		return m, clearStatusCmd()
	}

	m.status = indicatorAuto.Render(fmt.Sprintf("⚡ %s is now %d/%d in its rebase queue", key, j+1, len(queue)))
	next := m.autoRebaseCmd()

	return m, tea.Batch(clearStatusCmd(), next)
}
