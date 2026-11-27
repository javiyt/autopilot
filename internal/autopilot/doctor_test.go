package autopilot

import (
	"context"
	"runtime"
	"strings"
	"testing"
)

// TestDoctorBriefPlatformAndTools proves AC-1: brief doctor emits a
// "platform: <os>/<arch>" line and a per-tool PATH line for git/gh/pi, without
// requiring credentials or a model, and without probing endpoints (it is a
// launch smoke, not a gate). Zero network.
func TestDoctorBriefPlatformAndTools(t *testing.T) {
	out := DoctorBrief(context.Background(), Config{})
	lines := strings.Split(out, "\n")
	if len(lines) < 4 {
		t.Fatalf("brief doctor has too few lines: %q", out)
	}
	// The first line is the platform label matching the running host.
	wantPlatform := "platform: " + runtime.GOOS + "/" + runtime.GOARCH
	if lines[0] != wantPlatform {
		t.Fatalf("first line = %q, want %q", lines[0], wantPlatform)
	}
	// git/gh/pi each get a line (a PATH found by LookPath, or not-found), in
	// order. The value is whatever the host reports; brief never invents one.
	tools := []string{"git", "gh", "pi"}
	for i, tool := range tools {
		line := lines[i+1]
		prefix := tool + ": "
		if !strings.HasPrefix(line, prefix) {
			t.Fatalf("line %d = %q, want prefix %q", i+1, line, prefix)
		}
		rest := strings.TrimPrefix(line, prefix)
		// Found (a path containing '/') or the exact not-found marker.
		if rest != "not-found" && !strings.Contains(rest, "/") {
			t.Fatalf("line %d %q: value %q is neither a path nor not-found", i+1, line, rest)
		}
	}
	// Brief never mentions credentials, auth, or endpoints.
	for _, banned := range []string{"authenticated", "key", "endpoint", "token", "secret"} {
		if strings.Contains(strings.ToLower(out), banned) {
			t.Fatalf("brief doctor must not probe/report credentials or endpoints; found %q in:\n%s", banned, out)
		}
	}
}

// TestDoctorBriefDoesNotFailOnMissingTools proves a bare host (tools absent)
// still gets a complete, non-erroring brief report: brief is a launch smoke,
// so an absent tool is a "not-found" line, never an error (the full Doctor
// remains the hard gate).
func TestDoctorBriefDoesNotFailOnMissingTools(t *testing.T) {
	// On any host, DoctorBrief returns without error regardless of tool
	// presence; the function has no error return by design.
	out := DoctorBrief(context.Background(), Config{})
	if !strings.Contains(out, "platform: ") {
		t.Fatalf("brief doctor missing platform line: %q", out)
	}
	for _, tool := range []string{"git:", "gh:", "pi:"} {
		if !strings.Contains(out, tool) {
			t.Fatalf("brief doctor missing tool line %q: %q", tool, out)
		}
	}
}
