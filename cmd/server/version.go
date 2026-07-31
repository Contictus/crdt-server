package main

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// version is stamped at build time with -ldflags "-X main.version=v1.2.3".
//
// It is empty in an ordinary `go build`, and the fallback below is why: the Go
// toolchain already records the commit and whether the tree was dirty, so a
// binary built without the flag can still say what it is. A published image
// that cannot answer "which build is this" is an image nobody can correlate
// with an incident.
var version = ""

// buildVersion is what -version prints and what the startup log carries.
func buildVersion() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
			if len(revision) > 12 {
				revision = revision[:12]
			}
		case "vcs.modified":
			if s.Value == "true" {
				modified = "-dirty"
			}
		}
	}
	if revision == "" {
		// A `go run` has no VCS stamp at all, which is worth saying plainly
		// rather than dressing up as a version.
		return "devel"
	}
	return revision + modified
}

// versionLine is the whole of what -version prints: the build, the toolchain
// that produced it, and the platform. All three appear in bug reports.
func versionLine() string {
	return fmt.Sprintf("ycollab %s %s %s/%s",
		buildVersion(), runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
