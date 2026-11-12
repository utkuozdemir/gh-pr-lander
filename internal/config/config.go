// Package config holds the runtime configuration shared across the app.
package config

import "time"

// Config is the fully resolved runtime configuration. It is built once at
// startup (see package main) and passed by value into the components that
// need it. Every field has a sensible default applied before construction.
type Config struct {
	// Version says which build is running, for the page footer, the terminal
	// header and the log.
	Version               string
	Orgs                  []string
	PollInterval          time.Duration
	DataDir               string
	AutoMaxRetries        int
	AutoCooldown          time.Duration
	Notifications         bool
	RebaseCountdown       int
	MergedDisplayDuration time.Duration
	DeleteMergedBranches  bool
}

// RerunSettleGrace is how long, after triggering a CI re-run, the TUI keeps
// treating a PR as "not ready" even if GitHub momentarily reports all checks
// as passed. See internal/tui for why this exists (issue #1). It is derived
// from the poll interval so it always spans at least one poll cycle.
func (c Config) RerunSettleGrace() time.Duration {
	return max(c.PollInterval+time.Minute, 2*time.Minute)
}
