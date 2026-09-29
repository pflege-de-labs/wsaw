package main

import (
	"runtime/debug"
	"testing"
)

func TestWithVCSFallback(t *testing.T) {
	t.Parallel()

	const (
		revision = "c8615d6760769c8e75293c20e47a8718183644f5"
		vcsTime  = "2026-09-29T07:24:15Z"
		pseudo   = "v0.2.2-0.20260929072415-c8615d676076"
	)

	unstamped := buildInfo{version: unstampedVersion, commit: unstampedValue, date: unstampedValue}
	checkout := &debug.BuildInfo{
		Main: debug.Module{Version: pseudo},
		Settings: []debug.BuildSetting{
			{Key: "vcs", Value: "git"},
			{Key: "vcs.revision", Value: revision},
			{Key: "vcs.time", Value: vcsTime},
			{Key: "vcs.modified", Value: "false"},
		},
	}

	tests := []struct {
		name    string
		stamped buildInfo
		bi      *debug.BuildInfo
		want    buildInfo
	}{
		{
			name:    "checkout build takes the embedded VCS information",
			stamped: unstamped,
			bi:      checkout,
			want:    buildInfo{version: pseudo, commit: "c8615d676076", date: vcsTime},
		},
		{
			name:    "linker flags win over embedded information",
			stamped: buildInfo{version: "v0.3.0", commit: "abc1234", date: "2026-10-01T00:00:00Z"},
			bi:      checkout,
			want:    buildInfo{version: "v0.3.0", commit: "abc1234", date: "2026-10-01T00:00:00Z"},
		},
		{
			name:    "each unset value is filled independently",
			stamped: buildInfo{version: "v0.3.0", commit: unstampedValue, date: unstampedValue},
			bi:      checkout,
			want:    buildInfo{version: "v0.3.0", commit: "c8615d676076", date: vcsTime},
		},
		{
			name:    "go run embeds no VCS information and keeps the placeholders",
			stamped: unstamped,
			bi:      &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}},
			want:    unstamped,
		},
		{
			name:    "a short revision is kept whole",
			stamped: unstamped,
			bi:      &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abc"}}},
			want:    buildInfo{version: unstampedVersion, commit: "abc", date: unstampedValue},
		},
		{
			name:    "no build information at all",
			stamped: unstamped,
			bi:      nil,
			want:    unstamped,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := withVCSFallback(tt.stamped, tt.bi); got != tt.want {
				t.Errorf("withVCSFallback() = %+v, want %+v", got, tt.want)
			}
		})
	}
}
