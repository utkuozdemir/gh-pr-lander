package main

import (
	"os"
	"testing"
	"time"
)

func TestSaveLoadDBPersistsAutoRerunLast(t *testing.T) {
	dir := t.TempDir()
	last := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)

	if err := saveDB(
		dir,
		map[string]bool{"org/repo#1": true},
		map[string]bool{"org/repo#2": true},
		map[string]time.Time{
			"org/repo#2": last,
			"stale#3":    last.Add(time.Hour),
		},
	); err != nil {
		t.Fatalf("saveDB: %v", err)
	}

	autoMerge, autoRerun, autoRerunLast, err := loadDB(dir)
	if err != nil {
		t.Fatalf("loadDB: %v", err)
	}

	if !autoMerge["org/repo#1"] {
		t.Fatal("missing automerge entry")
	}
	if !autoRerun["org/repo#2"] {
		t.Fatal("missing autorerun entry")
	}
	if !autoRerunLast["org/repo#2"].Equal(last) {
		t.Fatalf("expected autorerun timestamp %s, got %s", last, autoRerunLast["org/repo#2"])
	}
	if _, ok := autoRerunLast["stale#3"]; ok {
		t.Fatal("stored timestamp for PR that is not in auto_rerun")
	}
}

func TestLoadDBReturnsMalformedYAMLError(t *testing.T) {
	dir := t.TempDir()

	if err := saveDB(dir, nil, nil, nil); err != nil {
		t.Fatalf("saveDB: %v", err)
	}
	if err := os.WriteFile(dbPath(dir), []byte("auto_merge: ["), 0o644); err != nil {
		t.Fatalf("write malformed db: %v", err)
	}

	if _, _, _, err := loadDB(dir); err == nil {
		t.Fatal("expected malformed YAML error")
	}
}
