package main

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"gopkg.in/yaml.v3"
)

// db.yaml structure — general-purpose persistence.
type db struct {
	AutoMerge []string `yaml:"auto_merge"`
}

func dbPath(dataDir string) string {
	return filepath.Join(dataDir, "db.yaml")
}

func loadAutoMerge(dataDir string) map[string]bool {
	result := make(map[string]bool)

	data, err := os.ReadFile(dbPath(dataDir))
	if err != nil {
		return result
	}

	var d db
	if err := yaml.Unmarshal(data, &d); err != nil {
		return result
	}

	for _, key := range d.AutoMerge {
		result[key] = true
	}

	return result
}

func saveAutoMerge(dataDir string, auto map[string]bool) error {
	keys := make([]string, 0, len(auto))
	for key, enabled := range auto {
		if enabled {
			keys = append(keys, key)
		}
	}

	sort.Strings(keys)

	d := db{AutoMerge: keys}

	data, err := yaml.Marshal(d)
	if err != nil {
		return err
	}

	return os.WriteFile(dbPath(dataDir), data, 0o644)
}

// autoMergeResultMsg is returned by autoMergeCmd.
type autoMergeResultMsg struct {
	prKey       string
	posted      bool // /m was posted
	mergeFailed bool // max retries exceeded — give up
	tooSoon     bool // last /m was within cooldown window
	err         error
}

// autoMergeCmd checks the /m comment count on a PR and posts if appropriate.
func autoMergeCmd(client *GitHubClient, pr PR, cfg Config) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		key := pr.RepoShort()

		count, lastMTime, err := client.countMergeComments(ctx, pr.Repo, pr.Number, cfg.MergeComment)
		if err != nil {
			return autoMergeResultMsg{prKey: key, err: err}
		}

		if count >= cfg.AutoMergeMaxRetries {
			return autoMergeResultMsg{prKey: key, mergeFailed: true}
		}

		if !lastMTime.IsZero() && time.Since(lastMTime) < cfg.AutoMergeCooldown {
			return autoMergeResultMsg{prKey: key, tooSoon: true}
		}

		err = client.postCommentRaw(ctx, pr.Repo, pr.Number, cfg.MergeComment)
		if err != nil {
			return autoMergeResultMsg{prKey: key, err: err}
		}

		return autoMergeResultMsg{prKey: key, posted: true}
	}
}
