package main_test

import (
	"context"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// -version has to print and exit, not start a server. It is the flag an
// operator reaches for while holding a container that is misbehaving, and one
// that opened a listener instead would be worse than not having it.
func TestVersionPrintsAndExits(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, buildServer(t), "-version").CombinedOutput()
	if err != nil {
		t.Fatalf("-version: %v\n%s", err, out)
	}
	line := strings.TrimSpace(string(out))

	// "ycollab <build> go1.x <os>/<arch>" - the build, the toolchain and the
	// platform, because all three end up in bug reports.
	want := regexp.MustCompile(`^ycollab \S+ go\d+\.\S+ \w+/\w+$`)
	if !want.MatchString(line) {
		t.Errorf("-version printed %q, which is not the documented shape", line)
	}
	// An unstamped build falls back to the VCS revision the toolchain records,
	// so "unknown" here means the fallback is broken rather than absent.
	if strings.Contains(line, "unknown") {
		t.Errorf("-version could not work out what this build is: %q", line)
	}
}

// The build appears on the startup log line, so correlating a log with an image
// never needs a second command.
func TestTheStartupLogCarriesTheBuild(t *testing.T) {
	srv := startServer(t, buildServer(t), freePort(t), "")
	if !strings.Contains(srv.logs.String(), "version=") {
		t.Errorf("the server did not log which build it is:\n%s", srv.logs.String())
	}
}
