package main

import (
	"runtime/debug"
	"strings"
)

// version and commit are set at build time for a release image
// (-ldflags "-X main.version=... -X main.commit=..."). Any other build reads
// what the Go toolchain recorded.
var (
	version string
	commit  string
)

// buildVersion says which build this is, for the page footer and the log:
// "v0.6.2 (5433d1f)" for a release, "dev (5433d1f, modified)" for a local
// build from a changed checkout, "dev" when nothing is known.
func buildVersion() string {
	v, c := version, commit
	modified := false

	if info, ok := debug.ReadBuildInfo(); ok {
		// Set by `go install module@version`.
		if v == "" && info.Main.Version != "" && info.Main.Version != "(devel)" {
			v = info.Main.Version
		}

		for _, setting := range info.Settings {
			switch setting.Key {
			case "vcs.revision":
				if c == "" {
					c = setting.Value
				}
			case "vcs.modified":
				modified = setting.Value == "true"
			}
		}
	}

	if v == "" {
		v = "dev"
	}

	var details []string

	if c != "" {
		details = append(details, c[:min(len(c), 7)])
	}

	// The toolchain already marks a version it derived from a changed tree.
	if modified && !strings.Contains(v, "dirty") {
		details = append(details, "modified")
	}

	if len(details) == 0 {
		return v
	}

	return v + " (" + strings.Join(details, ", ") + ")"
}
