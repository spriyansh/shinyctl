package cmd

import (
	"runtime/debug"
	"testing"
)

func TestVersionString(t *testing.T) {
	origVersion, origRead := Version, readBuildInfo
	t.Cleanup(func() { Version, readBuildInfo = origVersion, origRead })

	build := func(main string, settings ...string) func() (*debug.BuildInfo, bool) {
		return func() (*debug.BuildInfo, bool) {
			info := &debug.BuildInfo{Main: debug.Module{Version: main}}
			for i := 0; i+1 < len(settings); i += 2 {
				info.Settings = append(info.Settings, debug.BuildSetting{Key: settings[i], Value: settings[i+1]})
			}
			return info, true
		}
	}
	const rev = "0123456789abcdef0123456789abcdef01234567"
	cases := []struct {
		name    string
		version string
		read    func() (*debug.BuildInfo, bool)
		want    string
	}{
		{"set by the release build", "v0.1.0", build("(devel)"), "v0.1.0"},
		{"set by a snapshot build", "v0.1.0-snapshot-abc1234", nil, "v0.1.0-snapshot-abc1234"},
		{"go install at a tag", "", build("v0.1.0"), "v0.1.0"},
		{"a checkout at a tag", "", build("v0.1.0", "vcs.revision", rev, "vcs.modified", "false"), "v0.1.0"},
		{"a checkout past a tag (pseudo-version)", "", build("v0.1.1-0.20261028101500-0123456789ab", "vcs.revision", rev, "vcs.modified", "false"), "dev (0123456)"},
		{"a tagged checkout with changes", "", build("v0.1.0+dirty", "vcs.revision", rev, "vcs.modified", "true"), "dev (0123456, modified)"},
		{"go install @main, a pseudo-version after a tag", "", build("v0.1.1-0.20261028101500-0123456789ab"), "dev (0123456)"},
		{"go install @main, a pseudo-version of no tag", "", build("v0.0.0-20261028101500-0123456789ab"), "dev (0123456)"},
		{"a pseudo-version with changes", "", build("v0.0.0-20261028101500-0123456789ab+dirty"), "dev (0123456)"},
		{"a plain checkout", "", build("(devel)", "vcs.revision", rev, "vcs.modified", "false"), "dev (0123456)"},
		{"a checkout with changes", "", build("(devel)", "vcs.revision", rev, "vcs.modified", "true"), "dev (0123456, modified)"},
		{"no version control information", "", build("(devel)"), "dev"},
		{"no build information", "", func() (*debug.BuildInfo, bool) { return nil, false }, "dev"},
	}
	for _, c := range cases {
		Version = c.version
		if c.read != nil {
			readBuildInfo = c.read
		}
		if got := versionString(); got != c.want {
			t.Errorf("%s: versionString() = %q, want %q", c.name, got, c.want)
		}
	}
}
