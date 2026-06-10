package store

import (
	"os"
	"testing"
	"time"
)

func TestSaveLoadPersistsAutoRerunLast(t *testing.T) {
	dir := t.TempDir()
	last := time.Date(2026, 4, 14, 12, 0, 0, 0, time.UTC)

	if err := Save(dir, DB{
		AutoMerge: map[string]bool{"org/repo#1": true},
		AutoRerun: map[string]bool{"org/repo#2": true},
		AutoRerunLast: map[string]time.Time{
			"org/repo#2": last,
			"stale#3":    last.Add(time.Hour),
		},
	}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	db, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if !db.AutoMerge["org/repo#1"] {
		t.Fatal("missing automerge entry")
	}

	if !db.AutoRerun["org/repo#2"] {
		t.Fatal("missing autorerun entry")
	}

	if !db.AutoRerunLast["org/repo#2"].Equal(last) {
		t.Fatalf("expected autorerun timestamp %s, got %s", last, db.AutoRerunLast["org/repo#2"])
	}

	if _, ok := db.AutoRerunLast["stale#3"]; ok {
		t.Fatal("stored timestamp for PR that is not in auto_rerun")
	}
}

func TestLoadReturnsMalformedYAMLError(t *testing.T) {
	dir := t.TempDir()

	if err := Save(dir, DB{}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err := os.WriteFile(Path(dir), []byte("auto_merge: ["), 0o600); err != nil {
		t.Fatalf("write malformed db: %v", err)
	}

	if _, err := Load(dir); err == nil {
		t.Fatal("expected malformed YAML error")
	}
}
