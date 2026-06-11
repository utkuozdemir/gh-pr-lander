// Command sidero-rebaser is a terminal tool for the Siderolabs PR
// rebase-and-merge workflow. See README.md for the user-facing description and
// CLAUDE.md for the architecture and contributor notes.
//
// This file is intentionally thin: it parses flags, wires the components
// together, and runs the Bubble Tea program alongside a background poller under
// a structured-concurrency group. All behavior lives in internal/.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/sync/errgroup"

	"github.com/utkuozdemir/sidero-rebaser/internal/config"
	"github.com/utkuozdemir/sidero-rebaser/internal/github"
	"github.com/utkuozdemir/sidero-rebaser/internal/notify"
	"github.com/utkuozdemir/sidero-rebaser/internal/tui"
)

func main() {
	if err := run(); err != nil {
		log.Fatalf("failed to run: %v", err)
	}
}

func run() error {
	cfg, err := parseFlags()
	if err != nil {
		return err
	}

	logFile, err := setupLogging(cfg.DataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not set up log file: %v\n", err)
	}

	if logFile != nil {
		defer func() { _ = logFile.Close() }()
	}

	slog.Info("starting sidero-rebaser", "orgs", cfg.Orgs, "poll_interval", cfg.PollInterval, "data_dir", cfg.DataDir)

	notify.SetEnabled(cfg.Notifications)

	token, err := resolveGitHubToken()
	if err != nil {
		return err
	}

	client := github.New(token)

	userCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	if err := client.ResolveUsername(userCtx); err != nil {
		cancel()

		return fmt.Errorf("could not resolve GitHub username: %w", err)
	}

	cancel()

	// The poller fetches PRs in the background and streams results to the TUI;
	// the TUI can ask it to refresh immediately after an action.
	poller := github.NewPoller(client, cfg.Orgs, cfg.PollInterval)

	ctx, cancelPoller := context.WithCancel(context.Background())
	defer cancelPoller()

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return poller.Run(gctx)
	})

	m := tui.NewModel(cfg, client, poller.Results(), poller.Refresh)
	p := tea.NewProgram(m, tea.WithAltScreen())

	_, runErr := p.Run()

	// Stop the poller and wait for it to exit (structured concurrency).
	cancelPoller()

	if werr := g.Wait(); werr != nil && !errors.Is(werr, context.Canceled) {
		slog.Error("poller stopped with error", "error", werr)
	}

	if runErr != nil {
		return runErr
	}

	return nil
}

func parseFlags() (config.Config, error) {
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

	if autoMaxRetries < 1 {
		autoMaxRetries = 1
	}

	if autoCooldown < 0 {
		autoCooldown = 0
	}

	if rebaseCountdown < 0 {
		rebaseCountdown = 0
	}

	var parsedOrgs []string

	for o := range strings.SplitSeq(orgs, ",") {
		o = strings.TrimSpace(o)
		if o != "" {
			parsedOrgs = append(parsedOrgs, o)
		}
	}

	if len(parsedOrgs) == 0 {
		return config.Config{}, errors.New("no orgs specified")
	}

	return config.Config{
		Orgs:                  parsedOrgs,
		PollInterval:          pollInterval,
		DataDir:               dataDir,
		MergeComment:          mergeComment,
		AutoMaxRetries:        autoMaxRetries,
		AutoCooldown:          autoCooldown,
		Notifications:         notifications,
		RebaseCountdown:       rebaseCountdown,
		MergedDisplayDuration: mergedDisplayDuration,
		DeleteMergedBranches:  deleteMergedBranches,
	}, nil
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

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err == nil {
		token := strings.TrimSpace(string(out))
		if token != "" {
			return token, nil
		}
	}

	return "", errors.New(
		"no GitHub token found. set GITHUB_TOKEN, GH_TOKEN, or install gh CLI and run 'gh auth login'",
	)
}
