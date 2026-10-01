// Package notify sends desktop notifications. On macOS it uses the
// notification center via osascript; elsewhere it falls back to the terminal
// bell. Enable/disable is process-global because notifications are a
// cross-cutting side effect, not part of any component's state.
package notify

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"sync/atomic"
)

var enabled atomic.Bool

// Event is a kind of notification. Each has its own macOS sound, so they can be
// told apart without looking: bright for good news, low for a failure, an
// attention sound for automation that needs the user.
type Event struct {
	Title string
	Sound string // a name from /System/Library/Sounds
}

var (
	ReadyToMerge       = Event{"Ready to merge", "Glass"}
	MergePosted        = Event{"Merge posted", "Pop"}
	Merged             = Event{"Merged", "Hero"}
	Closed             = Event{"Closed without merge", "Bottle"}
	ChecksFailed       = Event{"Checks failed", "Basso"}
	NeedsRebase        = Event{"PR needs rebase", "Submarine"}
	AutorebaseNeedsYou = Event{"Autorebase needs you", "Sosumi"}
	AutomergeGaveUp    = Event{"Automerge gave up", "Funk"}
	AutorerunGaveUp    = Event{"Autorerun gave up", "Frog"}
)

// SetEnabled turns desktop notifications on or off.
func SetEnabled(v bool) {
	enabled.Store(v)
}

// Send shows a desktop notification for the event, if enabled.
func Send(event Event, body string) {
	if !enabled.Load() {
		return
	}

	// macOS notification center.
	script := fmt.Sprintf(`display notification %q with title %q sound name %q`, body, event.Title, event.Sound)

	_ = exec.CommandContext(context.Background(), "osascript", "-e", script).Run()

	// Terminal bell as fallback.
	_, _ = os.Stdout.WriteString("\a")
}
