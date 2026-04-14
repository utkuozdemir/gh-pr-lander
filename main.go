package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func main() {
	home, _ := os.UserHomeDir()
	defaultDataDir := filepath.Join(home, ".sidero-rebaser")

	var (
		orgs                  string
		pollInterval          time.Duration
		dataDir               string
		mergeComment          string
		autoMaxRetries        int
		autoCooldown          time.Duration
		notifications         bool
		rebaseCountdown       int
		mergedDisplayDuration time.Duration
		deleteMergedBranches  bool
	)

	flag.StringVar(&orgs, "orgs", "siderolabs,cosi-project", "comma-separated GitHub orgs to watch")
	flag.DurationVar(&pollInterval, "interval", 2*time.Minute, "poll interval (minimum 10s)")
	flag.StringVar(&dataDir, "data-dir", defaultDataDir, "data directory for repo clones, db, and logs")
	flag.StringVar(&mergeComment, "merge-comment", "/m", "comment to post when merging")
	flag.IntVar(&autoMaxRetries, "auto-max-retries", 3, "max auto attempts before giving up")
	flag.DurationVar(&autoCooldown, "auto-cooldown", 5*time.Minute, "cooldown between auto-merge/rerun attempts")
	flag.BoolVar(&notifications, "notifications", true, "enable desktop notifications")
	flag.IntVar(&rebaseCountdown, "rebase-countdown", 3, "seconds to show success before returning to TUI")
	flag.DurationVar(&mergedDisplayDuration, "merged-display-duration", 1*time.Hour, "how long to show merged PRs")
	flag.BoolVar(&deleteMergedBranches, "delete-merged-branches", true, "delete branch on fork after PR is merged")
	flag.Parse()

	if pollInterval < 10*time.Second {
		pollInterval = 10 * time.Second
	}

	cfg := Config{
		Orgs:                  strings.Split(orgs, ","),
		PollInterval:          pollInterval,
		DataDir:               dataDir,
		MergeComment:          mergeComment,
		AutoMaxRetries:        autoMaxRetries,
		AutoCooldown:          autoCooldown,
		Notifications:         notifications,
		RebaseCountdown:       rebaseCountdown,
		MergedDisplayDuration: mergedDisplayDuration,
		DeleteMergedBranches:  deleteMergedBranches,
	}

	logFile, err := setupLogging(cfg.DataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not set up log file: %v\n", err)
	}
	if logFile != nil {
		defer func() { _ = logFile.Close() }()
	}

	slog.Info("starting sidero-rebaser", "orgs", cfg.Orgs, "poll_interval", cfg.PollInterval, "data_dir", cfg.DataDir)

	token, err := resolveGitHubToken()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}

	notificationsEnabled = cfg.Notifications

	client := newGitHubClient(token)

	m := newModel(cfg, client)
	p := tea.NewProgram(m, tea.WithAltScreen())

	if _, err := p.Run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func setupLogging(dataDir string) (*os.File, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}

	logPath := filepath.Join(dataDir, "sidero-rebaser.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}

	handler := slog.NewTextHandler(f, &slog.HandlerOptions{Level: slog.LevelDebug})
	slog.SetDefault(slog.New(handler))

	return f, nil
}

func resolveGitHubToken() (string, error) {
	if token := os.Getenv("GITHUB_TOKEN"); token != "" {
		return token, nil
	}
	if token := os.Getenv("GH_TOKEN"); token != "" {
		return token, nil
	}

	out, err := exec.Command("gh", "auth", "token").Output()
	if err == nil {
		token := strings.TrimSpace(string(out))
		if token != "" {
			return token, nil
		}
	}

	return "", fmt.Errorf("no GitHub token found. set GITHUB_TOKEN, GH_TOKEN, or install gh CLI and run 'gh auth login'")
}
