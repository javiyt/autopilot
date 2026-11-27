package autopilot

import (
	"strings"
	"testing"
)

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

// TestValidateModelsForPlatformGuard checks the issue-#5 platform guard:
// omlx without base_url is macOS-only and must be rejected on any other host.
func TestValidateModelsForPlatformGuard(t *testing.T) {
	omlxLocal := map[string]ModelProfile{
		"plan": {Provider: "omlx", ID: "planner"},
	}
	// macOS hosts: omlx without base_url is the built-in MLX provider — legal.
	for _, platform := range []string{"macos-arm64", "macos-amd64"} {
		if err := validateModelsForPlatform(omlxLocal, platform); err != nil {
			t.Errorf("%s: local omlx must be allowed, got %v", platform, err)
		}
	}
	// Non-macOS hosts: omlx without base_url would silently target a local
	// runtime that does not exist — must fail loudly (issue #5).
	for _, platform := range []string{"linux", "unknown"} {
		err := validateModelsForPlatform(omlxLocal, platform)
		if err == nil {
			t.Errorf("%s: local omlx must be rejected", platform)
			continue
		}
		if !strings.Contains(err.Error(), "omlx without base_url is macOS-only") {
			t.Errorf("%s: error must be actionable, got %v", platform, err)
		}
		if !strings.Contains(err.Error(), "models.plan") {
			t.Errorf("%s: error must name the offending role, got %v", platform, err)
		}
	}
	// omlx WITH base_url is a remote endpoint — legal on every host.
	omlxRemote := map[string]ModelProfile{
		"plan": {Provider: "omlx", BaseURL: "https://mac-studio:8000/v1", ID: "planner"},
	}
	for _, platform := range []string{"linux", "macos-arm64", "unknown"} {
		if err := validateModelsForPlatform(omlxRemote, platform); err != nil {
			t.Errorf("%s: remote omlx must be allowed, got %v", platform, err)
		}
	}
	// Other providers are unaffected by the guard.
	other := map[string]ModelProfile{
		"plan": {Provider: "ollama", ID: "qwen"},
	}
	if err := validateModelsForPlatform(other, "linux"); err != nil {
		t.Errorf("ollama must be allowed on linux, got %v", err)
	}
}
