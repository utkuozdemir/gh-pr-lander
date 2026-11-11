// Command gh-pr-lander is a tool for a rebase-and-merge PR workflow with
// signed commits and fast-forward merges. See README.md for the user-facing
// description and AGENTS.md for the architecture and contributor notes.
//
// This file is intentionally thin: it parses flags, wires the components
// together, and runs the Bubble Tea program alongside a background poller under
// a structured-concurrency group. `gh-pr-lander serve` runs the server
// instead (serve.go). All behavior lives in internal/.
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

	"github.com/utkuozdemir/gh-pr-lander/internal/config"
	"github.com/utkuozdemir/gh-pr-lander/internal/github"
	"github.com/utkuozdemir/gh-pr-lander/internal/notify"
	"github.com/utkuozdemir/gh-pr-lander/internal/tui"
)

func main() {
	run, args := runTerminal, os.Args[1:]
	if len(args) > 0 && args[0] == "serve" {
		run, args = runServer, args[1:]
	}

	if err := run(args); err != nil {
		log.Fatalf("failed to run: %v", err)
	}
}

func runTerminal(args []string) error {
	cfg, _, err := parseFlags(args, false)
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

	slog.Info("starting gh-pr-lander", "orgs", cfg.Orgs, "poll_interval", cfg.PollInterval, "data_dir", cfg.DataDir)

	notify.SetEnabled(cfg.Notifications)

	token, err := resolveGitHubToken()
	if err != nil {
		return err
	}

	client, err := newClient(token)
	if err != nil {
		return err
	}

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

func newClient(token string) (*github.Client, error) {
	client := github.New(token)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	if err := client.ResolveUsername(ctx); err != nil {
		return nil, fmt.Errorf("could not resolve GitHub username: %w", err)
	}

	return client, nil
}

// serveOptions are the settings only the server has.
type serveOptions struct {
	listen    string
	tokenFile string
}

// parseFlags reads the flags of the terminal app, or of the server when serve
// is set. The two share most of them.
func parseFlags(args []string, serve bool) (config.Config, serveOptions, error) {
	home, _ := os.UserHomeDir()
	defaultDataDir := filepath.Join(home, ".gh-pr-lander")
	name := "gh-pr-lander"

	// The server keeps its own switches, so it must not share the db file of
	// a terminal app on the same machine.
	if serve {
		defaultDataDir = filepath.Join(defaultDataDir, "server")
		name += " serve"
	}

	fs := flag.NewFlagSet(name, flag.ExitOnError)

	var (
		opts                  serveOptions
		orgs                  string
		pollInterval          time.Duration
		dataDir               string
		autoMaxRetries        int
		autoCooldown          time.Duration
		notifications         bool
		rebaseCountdown       int
		mergedDisplayDuration time.Duration
		deleteMergedBranches  bool
	)

	fs.StringVar(&orgs, "orgs", "siderolabs,cosi-project", "comma-separated GitHub orgs to watch")
	fs.DurationVar(&pollInterval, "interval", 2*time.Minute, "poll interval (minimum 10s)")
	fs.StringVar(&dataDir, "data-dir", defaultDataDir, "data directory for the db, and for repo clones and logs")
	fs.IntVar(&autoMaxRetries, "auto-max-retries", 3, "max autorerun attempts before giving up")
	fs.DurationVar(&autoCooldown, "auto-cooldown", 5*time.Minute, "cooldown between auto-merge/rerun attempts")
	fs.DurationVar(&mergedDisplayDuration, "merged-display-duration", 1*time.Hour, "how long to show merged PRs")
	fs.BoolVar(&deleteMergedBranches, "delete-merged-branches", true, "delete branch on fork after PR is merged")

	if serve {
		fs.StringVar(&opts.listen, "listen", ":8080", "address the page is served on")
		fs.StringVar(
			&opts.tokenFile,
			"token-file",
			"",
			"file holding the GitHub token (default: GITHUB_TOKEN, GH_TOKEN or gh)",
		)
	} else {
		fs.BoolVar(&notifications, "notifications", true, "enable desktop notifications")
		fs.IntVar(&rebaseCountdown, "rebase-countdown", 3, "seconds to show success before returning to TUI")
	}

	_ = fs.Parse(args) // exits on error

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
		return config.Config{}, opts, errors.New("no orgs specified")
	}

	return config.Config{
		Orgs:                  parsedOrgs,
		PollInterval:          pollInterval,
		DataDir:               dataDir,
		AutoMaxRetries:        autoMaxRetries,
		AutoCooldown:          autoCooldown,
		Notifications:         notifications,
		RebaseCountdown:       rebaseCountdown,
		MergedDisplayDuration: mergedDisplayDuration,
		DeleteMergedBranches:  deleteMergedBranches,
	}, opts, nil
}

func setupLogging(dataDir string) (*os.File, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}

	logPath := filepath.Join(dataDir, "gh-pr-lander.log")

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
