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

// SetEnabled turns desktop notifications on or off.
func SetEnabled(v bool) {
	enabled.Store(v)
}

// Send shows a desktop notification, if enabled.
func Send(title, body string) {
	if !enabled.Load() {
		return
	}

	// macOS notification center.
	script := fmt.Sprintf(`display notification %q with title %q sound name "Glass"`, body, title)

	_ = exec.CommandContext(context.Background(), "osascript", "-e", script).Run()

	// Terminal bell as fallback.
	_, _ = os.Stdout.WriteString("\a")
}
