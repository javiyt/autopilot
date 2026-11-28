package autopilot

import (
	"runtime"
	"strings"
)

// platformLabel renders the host (or a given GOOS/GOARCH pair) as the
// "<os>/<arch>" string used in doctor output and remote-check evidence, so
// an ARM64 Linux host reads as "linux/arm64" and a macOS host as
// "darwin/arm64". It is a pure helper: tests exercise every target without
// depending on the build system, mirroring detectPlatformFrom.
func platformLabelFrom(goos, goarch string) string {
	return strings.ToLower(goos) + "/" + strings.ToLower(goarch)
}

// platformLabel returns "<os>/<arch>" for the running host.
func platformLabel() string {
	return platformLabelFrom(runtime.GOOS, runtime.GOARCH)
}

// validArch is the closed set of architecture constraints a check may
// declare. "any" is the default (every host); anything else must name one of
// the two supported architectures. The enum is the guard against a
// host-controlled or model-emitted string turning into a shell value.
func validArch(a string) bool {
	switch a {
	case "", "any", "amd64", "arm64":
		return true
	}
	return false
}

// platformAllows reports whether the running host natively satisfies a check's
// architecture constraint. An empty/"any" constraint matches every host
// (no arch restriction); a concrete constraint matches only when the host
// architecture equals it. Case is normalized so "ARM64" and "arm64" agree.
// The helper is pure (takes the host arch as an argument) so tests can
// exercise every branch without depending on the build system.
func platformAllows(hostArch, want string) bool {
	if want == "" || want == "any" {
		return true
	}
	return strings.ToLower(hostArch) == strings.ToLower(want)
}
