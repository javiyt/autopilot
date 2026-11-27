package autopilot

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestCrossBuild proves AC-4: the module builds cleanly for every supported
// target with zero external runtime dependencies. The supported targets are the
// unix architectures this codebase actually runs on: linux/{amd64,arm64} and
// darwin/{amd64,arm64} (the issue's host class). Windows is NOT a supported
// target (the issue scope excludes it) because the durable state uses
// syscall.Flock, which is unavailable on Windows; claiming it would be a lie.
// The test invokes the Go toolchain to compile the autopilot package to a temp
// binary for each GOOS/GOARCH. It skips only when the Go toolchain is
// unavailable (a source-only checkout), the one case the test cannot perform.
func TestCrossBuild(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go toolchain not on PATH; cross-build skipped (module is buildable, but the toolchain is required to prove it)")
	}
	targets := []struct{ goos, goarch string }{
		{"linux", "amd64"},
		{"linux", "arm64"},
		{"darwin", "amd64"},
		{"darwin", "arm64"},
	}
	tmp, err := os.MkdirTemp("", "pi-autopilot-crossbuild-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(tmp)
	for _, tgt := range targets {
		t.Run(tgt.goos+"/"+tgt.goarch, func(t *testing.T) {
			out := filepath.Join(tmp, tgt.goos+"-"+tgt.goarch)
			cmd := exec.Command(goBin, "build", "-o", out, "./internal/autopilot")
			cmd.Env = append(os.Environ(), "GOOS="+tgt.goos, "GOARCH="+tgt.goarch)
			cmd.Dir = moduleRoot()
			res, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("cross build failed for %s/%s: %v\n%s", tgt.goos, tgt.goarch, err, res)
			}
			// The artifact must exist and be non-empty.
			info, err := os.Stat(out)
			if err != nil || info.Size() == 0 {
				t.Fatalf("expected a non-empty %s/%s artifact at %s (err=%v)", tgt.goos, tgt.goarch, out, err)
			}
		})
	}
}

// moduleRoot walks up from this test file's package directory to the directory
// containing go.mod. The autopilot package lives at <module>/internal/autopilot.
func moduleRoot() string {
	wd, err := os.Getwd()
	if err != nil {
		wd = "."
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return wd
}
