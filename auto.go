package main

import (
	"context"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"gopkg.in/yaml.v3"
)

// db.yaml — just tracks which PRs have auto features enabled.
// All retry counts come from the GitHub API (comments for merge, run_attempt for reruns).
type dbData struct {
	AutoMerge     []string             `yaml:"auto_merge,omitempty"`
	AutoRerun     []string             `yaml:"auto_rerun,omitempty"`
	AutoRerunLast map[string]time.Time `yaml:"auto_rerun_last,omitempty"`
}

func dbPath(dataDir string) string {
	return filepath.Join(dataDir, "db.yaml")
}

func loadDB(dataDir string) (autoMerge, autoRerun map[string]bool, autoRerunLast map[string]time.Time, err error) {
	autoMerge = make(map[string]bool)
	autoRerun = make(map[string]bool)
	autoRerunLast = make(map[string]time.Time)

	data, err := os.ReadFile(dbPath(dataDir))
	if err != nil {
		if os.IsNotExist(err) {
			err = nil
		}
		return
	}

	var d dbData
	if err = yaml.Unmarshal(data, &d); err != nil {
		return
	}

	for _, key := range d.AutoMerge {
		autoMerge[key] = true
	}
	for _, key := range d.AutoRerun {
		autoRerun[key] = true
	}
	maps.Copy(autoRerunLast, d.AutoRerunLast)

	return
}

func saveDB(dataDir string, autoMerge, autoRerun map[string]bool, autoRerunLast map[string]time.Time) error {
	var d dbData

	for key := range autoMerge {
		d.AutoMerge = append(d.AutoMerge, key)
	}
	sort.Strings(d.AutoMerge)

	for key := range autoRerun {
		d.AutoRerun = append(d.AutoRerun, key)
	}
	sort.Strings(d.AutoRerun)
	if len(autoRerunLast) > 0 {
		d.AutoRerunLast = make(map[string]time.Time, len(autoRerunLast))
		for key, last := range autoRerunLast {
			if autoRerun[key] && !last.IsZero() {
				d.AutoRerunLast[key] = last
			}
		}
	}

	data, err := yaml.Marshal(d)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(dataDir, ".db.yaml.*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmpName, dbPath(dataDir))
}

// Messages

type autoMergeResultMsg struct {
	prKey   string
	posted  bool
	failed  bool // max retries exceeded
	tooSoon bool
	err     error
}

type autoRerunResultMsg struct {
	prKey       string
	reran       bool
	failed      bool // max retries exceeded
	unavailable bool // no failed GitHub Actions runs exist for this SHA
	reranAt     time.Time
	err         error
}

// Commands

// autoMergeCmd checks /m comment count from the GitHub API, then posts if appropriate.
func autoMergeCmd(client *GitHubClient, pr PR, cfg Config) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		key := pr.RepoShort()

		count, lastTime, err := client.countMergeComments(ctx, pr.Repo, pr.Number, cfg.MergeComment)
		if err != nil {
			return autoMergeResultMsg{prKey: key, err: err}
		}

		if count >= cfg.AutoMaxRetries {
			return autoMergeResultMsg{prKey: key, failed: true}
		}

		if !lastTime.IsZero() && time.Since(lastTime) < cfg.AutoCooldown {
			return autoMergeResultMsg{prKey: key, tooSoon: true}
		}

		if err := client.postCommentRaw(ctx, pr.Repo, pr.Number, cfg.MergeComment); err != nil {
			return autoMergeResultMsg{prKey: key, err: err}
		}

		return autoMergeResultMsg{prKey: key, posted: true}
	}
}

// autoRerunCmd checks run_attempt from the GitHub API, then reruns if appropriate.
func autoRerunCmd(client *GitHubClient, pr PR, maxRetries int) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		key := pr.RepoShort()

		summary, err := client.failedWorkflowRunsSummary(ctx, pr.Repo, pr.HeadSHA)
		if err != nil {
			return autoRerunResultMsg{prKey: key, err: err}
		}
		if summary.Count == 0 {
			return autoRerunResultMsg{prKey: key, unavailable: true}
		}

		// run_attempt=1 is the original run, so run_attempt > maxRetries means we've rerun maxRetries times
		if summary.MaxAttempt > maxRetries {
			return autoRerunResultMsg{prKey: key, failed: true}
		}

		if err := client.rerunFailedJobs(ctx, pr.Repo, pr.HeadSHA); err != nil {
			return autoRerunResultMsg{prKey: key, err: err}
		}

		return autoRerunResultMsg{prKey: key, reran: true, reranAt: time.Now()}
	}
}
