package main

import (
	"runtime/debug"
	"testing"
)

func TestDescribeBuild(t *testing.T) {
	vcs := func(revision, modified string) []debug.BuildSetting {
		return []debug.BuildSetting{{Key: "vcs.revision", Value: revision}, {Key: "vcs.modified", Value: modified}}
	}

	for _, tt := range []struct {
		name    string
		version string
		commit  string
		info    *debug.BuildInfo
		want    string
	}{
		{
			name:    "release image, version and commit from ldflags",
			version: "v0.8.0", commit: "beb41ee0123456789",
			info: &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}},
			want: "v0.8.0 (beb41ee)",
		},
		{
			name: "go install module@version",
			info: &debug.BuildInfo{Main: debug.Module{Version: "v0.8.0"}},
			want: "v0.8.0",
		},
		{
			name: "local build on a tagged commit",
			info: &debug.BuildInfo{Main: debug.Module{Version: "v0.8.0"}, Settings: vcs("beb41ee0123456789", "false")},
			want: "v0.8.0 (beb41ee)",
		},
		{
			name: "local build on a changed tagged checkout",
			info: &debug.BuildInfo{Main: debug.Module{Version: "v0.8.0+dirty"}, Settings: vcs("beb41ee0123456789", "true")},
			want: "dev (beb41ee, modified)",
		},
		{
			name: "local build past the last tag gets a made-up version from the toolchain",
			info: &debug.BuildInfo{
				Main:     debug.Module{Version: "v0.8.1-0.20261112003422-b87080ac9b0d"},
				Settings: vcs("b87080ac9b0d0123", "false"),
			},
			want: "dev (b87080a)",
		},
		{name: "nothing known", info: nil, want: "dev"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := describeBuild(tt.version, tt.commit, tt.info); got != tt.want {
				t.Fatalf("expected %q, got %q", tt.want, got)
			}
		})
	}
}
