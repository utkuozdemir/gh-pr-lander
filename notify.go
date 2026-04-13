package main

import (
	"fmt"
	"os/exec"
)

var notificationsEnabled = true

func sendNotification(title, body string) {
	if !notificationsEnabled {
		return
	}

	// macOS notification center
	script := fmt.Sprintf(`display notification %q with title %q sound name "Glass"`, body, title)
	//nolint:gosec
	_ = exec.Command("osascript", "-e", script).Run()

	// Terminal bell as fallback
	fmt.Print("\a")
}
