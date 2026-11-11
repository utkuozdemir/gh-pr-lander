package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/utkuozdemir/gh-pr-lander/internal/github"
	"github.com/utkuozdemir/gh-pr-lander/internal/notify"
	"github.com/utkuozdemir/gh-pr-lander/internal/server"
)

// runServer runs the tool with no terminal: the poller, the engine's
// automation and the page, until it is told to stop.
func runServer(args []string) error {
	cfg, opts := parseFlags(args, true)

	// A server logs where its supervisor collects output, not into a file that
	// grows in the data directory, and there is no desktop to notify.
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	notify.SetEnabled(false)

	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return err
	}

	token, err := readToken(opts.tokenFile)
	if err != nil {
		return err
	}

	client, err := newClient(token)
	if err != nil {
		return err
	}

	poller := github.NewPoller(client, cfg.Orgs, cfg.PollInterval)

	srv, err := server.New(cfg, client, poller.Refresh)
	if err != nil {
		return fmt.Errorf("could not load %s: %w", cfg.DataDir, err)
	}

	httpServer := &http.Server{
		Addr:              opts.listen,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("starting server", "user", client.Username(), "orgs", cfg.Orgs, "listen", opts.listen,
		"poll_interval", cfg.PollInterval, "data_dir", cfg.DataDir)

	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error { return poller.Run(gctx) })
	g.Go(func() error { return srv.Run(gctx, poller.Results()) })
	g.Go(func() error {
		if err := httpServer.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			return err
		}

		return nil
	})
	g.Go(func() error {
		<-gctx.Done()

		// A merge started from the page gets to finish.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()

		return httpServer.Shutdown(shutdownCtx)
	})

	if err := g.Wait(); err != nil && !errors.Is(err, context.Canceled) {
		return err
	}

	return nil
}

func readToken(path string) (string, error) {
	if path == "" {
		return resolveGitHubToken()
	}

	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("could not read the token file: %w", err)
	}

	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("the token file %s is empty", path)
	}

	return token, nil
}
