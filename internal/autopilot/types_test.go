package autopilot

import "testing"

func TestDetectPlatformFrom(t *testing.T) {
	tests := []struct {
		name   string
		goos   string
		goarch string
		want   string
	}{
		{"darwin arm64", "darwin", "arm64", "macos-arm64"},
		{"darwin aarch64", "darwin", "aarch64", "macos-arm64"},
		{"darwin amd64", "darwin", "amd64", "macos-amd64"},
		{"darwin 386", "darwin", "386", "macos-amd64"},
		{"linux amd64", "linux", "amd64", "linux"},
		{"linux arm64", "linux", "arm64", "linux"},
		{"linux 386", "linux", "386", "linux"},
		{"windows amd64", "windows", "amd64", "unknown"},
		{"windows arm64", "windows", "arm64", "unknown"},
		{"freebsd amd64", "freebsd", "amd64", "unknown"},
		{"Darwin ARM64 (mixed case)", "Darwin", "ARM64", "macos-arm64"},
		{"LINUX amd64 (upper case)", "LINUX", "amd64", "linux"},
		{"empty", "", "", "unknown"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := detectPlatformFrom(tt.goos, tt.goarch)
			if got != tt.want {
				t.Errorf("detectPlatformFrom(%q, %q) = %q, want %q", tt.goos, tt.goarch, got, tt.want)
			}
		})
	}
}

// TestDetectPlatform verifies the real detectPlatform() returns a valid value
// on the host it runs on. It does not assert a specific value since the host
// varies.
func TestDetectPlatform(t *testing.T) {
	got := detectPlatform()
	valid := map[string]bool{
		"macos-arm64": true,
		"macos-amd64": true,
		"linux":       true,
		"unknown":     true,
	}
	if !valid[got] {
		t.Errorf("detectPlatform() = %q, not in valid set %v", got, valid)
	}
}
