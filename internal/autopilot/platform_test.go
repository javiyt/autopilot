package autopilot

import (
	"runtime"
	"strings"
	"testing"
)

// TestPlatformLabelFrom covers the pure label helper across every supported
// target (AC-4) plus case normalization.
func TestPlatformLabelFrom(t *testing.T) {
	cases := []struct {
		goos, goarch, want string
	}{
		{"linux", "amd64", "linux/amd64"},
		{"linux", "arm64", "linux/arm64"},
		{"darwin", "arm64", "darwin/arm64"},
		{"darwin", "amd64", "darwin/amd64"},
		{"LINUX", "ARM64", "linux/arm64"},
		{"Darwin", "Amd64", "darwin/amd64"},
	}
	for _, tc := range cases {
		if got := platformLabelFrom(tc.goos, tc.goarch); got != tc.want {
			t.Errorf("platformLabelFrom(%q, %q) = %q, want %q", tc.goos, tc.goarch, got, tc.want)
		}
	}
	// The real helper reports the host it runs on (no specific value asserted;
	// the host varies).
	if got := platformLabel(); got != platformLabelFrom(runtime.GOOS, runtime.GOARCH) {
		t.Errorf("platformLabel() = %q, host pair %q/%q", got, runtime.GOOS, runtime.GOARCH)
	}
}

// TestValidArch is the enum guard: only the closed set is legal; any other
// string is rejected so it can never become a shell value (AC-6 security).
func TestValidArch(t *testing.T) {
	for _, ok := range []string{"", "any", "amd64", "arm64"} {
		if !validArch(ok) {
			t.Errorf("validArch(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"x86_64", "aarch64", "386", "anyone", "am d64", "arm64;", "" + "`"} {
		if validArch(bad) {
			t.Errorf("validArch(%q) = true, want false", bad)
		}
	}
}

// TestPlatformAllows covers AC-3 at the pure layer: a concrete constraint the
// host cannot satisfy returns false (-> BLOCKED), "any" and a matching arch
// return true. Host arch is the argument so every branch is exercised.
func TestPlatformAllows(t *testing.T) {
	cases := []struct {
		host, want string
		ok         bool
	}{
		{"amd64", "any", true},
		{"amd64", "", true},
		{"arm64", "any", true},
		{"amd64", "amd64", true},
		{"amd64", "ARM64", false},
		{"amd64", "arm64", false},
		{"arm64", "amd64", false},
		{"arm64", "arm64", true},
		{"arm64", "Arm64", true},
		{"arm64", "Amd64", false},
	}
	for _, tc := range cases {
		if got := platformAllows(tc.host, tc.want); got != tc.ok {
			t.Errorf("platformAllows(%q, %q) = %t, want %t", tc.host, tc.want, got, tc.ok)
		}
	}
}

// TestArchBlockedCheckBlocksPhase proves AC-3: a check whose arch constraint the
// host cannot run natively is blocked (not executed) with a non-zero sentinel
// and a bounded cause, and a check whose constraint matches the host (or "any")
// is never blocked. The direction is symmetric (arm64 blocks amd64 and vice
// versa).
func TestArchBlockedCheckBlocksPhase(t *testing.T) {
	// A constraint matching the host is never blocked.
	if shouldBlockArch(runtime.GOARCH) {
		t.Fatalf("a check constrained to the host arch %s must not be blocked", runtime.GOARCH)
	}
	// "any" and empty never block.
	if shouldBlockArch("any") || shouldBlockArch("") {
		t.Fatal("'any'/empty arch must never block")
	}
	// A constraint on the OTHER arch is blocked, and the block is non-zero with
	// the arch-mismatch cause (not executed, never a silent skip).
	other := "arm64"
	if other == runtime.GOARCH {
		other = "amd64"
	}
	if !shouldBlockArch(other) {
		t.Fatalf("a check requiring %s on host %s must be blocked", other, runtime.GOARCH)
	}
	res := archBlockedResult(runtime.GOARCH, other)
	if res.ExitCode == 0 {
		t.Fatal("a blocked check must be non-zero; a silent skip is indistinguishable from failure")
	}
	if !res.Blocked || res.Cause != "arch-mismatch" {
		t.Fatalf("a blocked check must carry Blocked=true + cause arch-mismatch, got %+v", res)
	}
	if res.TimedOut {
		t.Fatal("a blocked check was not executed; it must not be marked timed out")
	}
	if !strings.Contains(res.Output, other) || !strings.Contains(res.Output, runtime.GOARCH) {
		t.Fatalf("blocked output must name the required and host arch: %q", res.Output)
	}
}
