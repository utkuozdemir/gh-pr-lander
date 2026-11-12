package main

import (
	"regexp"
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

// releaseVersion matches a version that is a plain release, such as v0.8.0.
var releaseVersion = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// buildVersion says which build this is, for the page footer, the terminal
// header and the log.
func buildVersion() string {
	info, _ := debug.ReadBuildInfo()

	return describeBuild(version, commit, info)
}

// describeBuild puts a build into words: "v0.8.0 (beb41ee)" for a release,
// "dev (beb41ee, modified)" for a build from a changed checkout, "dev" when
// nothing is known.
//
// The toolchain fills in a version for every build from a checkout. Away from
// a tag that is a made-up one, such as v0.8.1-0.20261112003422-b87080ac9b0d,
// which names a release that does not exist. Only a plain release version is
// shown, anything else is "dev" with the commit.
func describeBuild(v, c string, info *debug.BuildInfo) string {
	modified := false

	if info != nil {
		if v == "" {
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

	if !releaseVersion.MatchString(v) {
		v = "dev"
	}

	var details []string

	if c != "" {
		details = append(details, c[:min(len(c), 7)])
	}

	if modified {
		details = append(details, "modified")
	}

	if len(details) == 0 {
		return v
	}

	return v + " (" + strings.Join(details, ", ") + ")"
}
