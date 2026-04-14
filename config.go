package main

import "time"

type Config struct {
	Orgs                  []string
	PollInterval          time.Duration
	DataDir               string
	AutoMaxRetries        int
	AutoCooldown          time.Duration
	MergeComment          string
	Notifications         bool
	RebaseCountdown       int
	MergedDisplayDuration time.Duration
	DeleteMergedBranches  bool
}
