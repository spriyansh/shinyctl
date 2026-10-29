/*
Copyright © 2026 Priyansh Srivastava <priyansh@metapriyansh.com>
*/
package cmd

import (
	"fmt"
	"regexp"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// Version is set by the release build (-X github.com/spriyansh/shinyctl/cmd.Version=v0.1.0).
// When it is empty the version comes from the build's own information.
var Version = ""

// readBuildInfo is debug.ReadBuildInfo; the tests replace it.
var readBuildInfo = debug.ReadBuildInfo

// pseudoHash picks the commit out of a pseudo-version, which a build made
// with `go install ...@main` has and no version control stamp.
var pseudoHash = regexp.MustCompile(`[-.]\d{14}-([0-9a-f]{12})(\+dirty)?$`)

// notARelease matches the versions Go makes up for a commit that has no tag
// (v0.0.0-20261028101500-abcdef123456, v0.1.1-0.20261028101500-abcdef123456)
// and for a tagged checkout with uncommitted changes (v0.1.0+dirty).
var notARelease = regexp.MustCompile(`[-.]\d{14}-[0-9a-f]{12}|\+dirty$`)

// versionString says which version this is, in this order:
//  1. the version the release build set;
//  2. the module version of a `go install ...@v0.1.0` build, or of a checkout
//     at a tag (Go 1.24 and later stamp it), when it is a release version;
//  3. "dev (abc1234)" for a build from a checkout, with ", modified" when it
//     had uncommitted changes, or for a build of an untagged commit made with
//     go install (its pseudo-version names the commit);
//  4. "dev".
func versionString() string {
	if Version != "" {
		return Version
	}
	info, ok := readBuildInfo()
	if !ok {
		return "dev"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" && !notARelease.MatchString(v) {
		return v
	}
	var revision string
	var modified bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value == "true"
		}
	}
	if revision == "" {
		// A build in module mode (go install ...@main) has no stamp, but its
		// pseudo-version names the commit.
		if m := pseudoHash.FindStringSubmatch(info.Main.Version); m != nil {
			revision = m[1]
		}
	}
	if revision == "" {
		return "dev"
	}
	if len(revision) > 7 {
		revision = revision[:7]
	}
	if modified {
		return fmt.Sprintf("dev (%s, modified)", revision)
	}
	return fmt.Sprintf("dev (%s)", revision)
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print the shinyctl version",
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println(versionString())
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
