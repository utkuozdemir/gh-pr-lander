package notify

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestEventSoundsAreDistinctAndExist(t *testing.T) {
	events := []Event{
		ReadyToMerge, Merged, Closed, ChecksFailed,
		NeedsRebase, AutorebaseNeedsYou, AutomergeGaveUp, AutorerunGaveUp,
	}

	seen := make(map[string]string)

	for _, e := range events {
		if other, ok := seen[e.Sound]; ok {
			t.Errorf("%q and %q share the sound %q", e.Title, other, e.Sound)
		}

		seen[e.Sound] = e.Title

		// A misspelled sound name would just go unnoticed.
		if runtime.GOOS == "darwin" {
			if _, err := os.Stat(filepath.Join("/System/Library/Sounds", e.Sound+".aiff")); err != nil {
				t.Errorf("%q: sound %q not found: %v", e.Title, e.Sound, err)
			}
		}
	}
}
