package tui

import "github.com/utkuozdemir/gh-pr-lander/internal/engine"

// Client is the slice of the GitHub API the TUI depends on, the same one the
// engine uses. *github.Client satisfies it.
type Client = engine.Client
