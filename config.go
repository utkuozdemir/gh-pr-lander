package main

import "time"

type Config struct {
	Orgs                []string
	PollInterval        time.Duration
	DataDir             string
	AutoMergeMaxRetries int
	AutoMergeCooldown   time.Duration
	MergeComment        string
	Notifications       bool
	RebaseCountdown     int
}
