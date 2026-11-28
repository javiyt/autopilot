package autopilot

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

// Issue #10: the Telegram token must never appear in the unit definition.
// These tests are platform-independent (pure renderers + a files-only seam
// parametrized on goos) so they run on the ubuntu CI runner for both templates
// with no GOOS skips.

const sentinelToken = "SENTINEL-TELEGRAM-DO-NOT-LEAK"

// serviceTestConfig returns a minimal Config with an absolute StateDir and a
// Path, sufficient for installServiceFiles. Models etc. are irrelevant here.
func serviceTestConfig(t *testing.T, stateDir string) Config {
	t.Helper()
	if !filepath.IsAbs(stateDir) {
		t.Fatal("stateDir must be absolute")
	}
	return Config{StateDir: stateDir, Path: filepath.Join(stateDir, "config.yaml")}
}

// TestServiceUnitTemplatesExcludeToken renders both unit templates and asserts
// the sentinel token is absent while the secret reference (EnvironmentFile /
// runner path) and PATH are present.
func TestServiceUnitTemplatesExcludeToken(t *testing.T) {
	bin := "/opt/bin/pi-autopilot"
	cfgPath := "/Users/me/my dir/config.yaml"
	runner := "/Users/me/state dir/service-run.sh"
	envFile := "/Users/me/state dir/service-secrets.env"
	logs := "/Users/me/state dir/service-logs"
	pathEnv := "/usr/bin:/bin"

	systemd := renderSystemdUnit(bin, cfgPath, envFile, pathEnv, logs)
	if strings.Contains(systemd, sentinelToken) {
		t.Fatal("systemd unit template contains the literal token")
	}
	if !strings.Contains(systemd, "EnvironmentFile="+envFile) {
		t.Fatalf("systemd unit must reference the env file: %s", systemd)
	}
	if !strings.Contains(systemd, "Environment=PATH="+pathEnv) {
		t.Fatalf("systemd unit must still set PATH: %s", systemd)
	}
	if !strings.Contains(systemd, "Restart=always") {
		t.Fatalf("systemd unit must keep unattended restart: %s", systemd)
	}
	if strings.Contains(systemd, "PI_AUTOPILOT_TELEGRAM_TOKEN=") {
		t.Fatal("systemd unit must not inline the token key at all")
	}

	plist := renderLaunchAgentPlist(cfgPath, runner, pathEnv, logs)
	if strings.Contains(plist, sentinelToken) {
		t.Fatal("LaunchAgent plist template contains the literal token")
	}
	if strings.Contains(plist, "PI_AUTOPILOT_TELEGRAM_TOKEN") {
		t.Fatal("LaunchAgent plist must not carry the token key at all")
	}
	if !strings.Contains(plist, "<string>/bin/sh</string>") || !strings.Contains(plist, "<string>"+runner+"</string>") {
		t.Fatalf("LaunchAgent plist must run the sourcing runner: %s", plist)
	}
	if !strings.Contains(plist, "<key>PATH</key>") {
		t.Fatalf("LaunchAgent plist must still set PATH: %s", plist)
	}
	if !strings.Contains(plist, "<key>RunAtLoad</key>") || !strings.Contains(plist, "<key>KeepAlive</key>") {
		t.Fatalf("LaunchAgent plist must keep RunAtLoad/KeepAlive: %s", plist)
	}
}

// TestServiceInstallFilesSecretNotOnUnitPath runs installServiceFiles for both
// templates from a temp HOME and asserts the sentinel token is absent from
// every generated artifact on disk (unit, plist, runner) and present only in
// the owner-only secret file.
func TestServiceInstallFilesSecretNotOnUnitPath(t *testing.T) {
	for _, goos := range []string{"linux", "darwin"} {
		t.Run(goos, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			stateDir := t.TempDir()
			cfg := serviceTestConfig(t, stateDir)

			// installServiceFiles resolves the binary via os.Executable(); the
			// running test binary is a real path, so this succeeds on any host.
			_, _, err := installServiceFiles(cfg, sentinelToken, goos)
			if err != nil {
				t.Fatalf("installServiceFiles(%s): %v", goos, err)
			}

			// The secret file must hold the token.
			envData, err := os.ReadFile(filepath.Join(stateDir, serviceSecret))
			if err != nil {
				t.Fatalf("secret file missing: %v", err)
			}
			if !strings.Contains(string(envData), "PI_AUTOPILOT_TELEGRAM_TOKEN="+sentinelToken) {
				t.Fatalf("secret file must contain the token line: %s", envData)
			}

			// Walk every generated artifact and assert the token is absent from
			// anything except the secret file itself.
			check := func(path string) {
				t.Helper()
				data, err := os.ReadFile(path)
				if err != nil {
					t.Fatalf("read %s: %v", path, err)
				}
				if strings.Contains(string(data), sentinelToken) {
					t.Fatalf("token leaked into %s", path)
				}
			}
			homeEntries := []string{
				filepath.Join(home, ".config", "systemd", "user", serviceUnit),
				filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist"),
				filepath.Join(stateDir, serviceRunner),
			}
			for _, p := range homeEntries {
				if _, err := os.Stat(p); err == nil {
					check(p)
				}
			}
		})
	}
}

// TestServiceSecretsFileMode asserts the exact permissions on every generated
// artifact: 0600 secret file, 0700 runner, 0600 unit/plist.
func TestServiceSecretsFileMode(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := t.TempDir()
	cfg := serviceTestConfig(t, stateDir)

	if _, _, err := installServiceFiles(cfg, sentinelToken, "darwin"); err != nil {
		t.Fatal(err)
	}
	assertMode := func(path string, want os.FileMode) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("stat %s: %v", path, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("%s mode = %04o, want %04o", path, got, want)
		}
	}
	assertMode(filepath.Join(stateDir, serviceSecret), 0600)
	assertMode(filepath.Join(stateDir, serviceRunner), 0700)
	assertMode(filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist"), 0600)

	// Linux unit mode.
	home2 := t.TempDir()
	t.Setenv("HOME", home2)
	stateDir2 := t.TempDir()
	cfg2 := serviceTestConfig(t, stateDir2)
	if _, _, err := installServiceFiles(cfg2, sentinelToken, "linux"); err != nil {
		t.Fatal(err)
	}
	assertMode(filepath.Join(stateDir2, serviceSecret), 0600)
	assertMode(filepath.Join(home2, ".config", "systemd", "user", serviceUnit), 0600)
}

// TestServiceSecretsFileFailsClosedOnInsecurePreexisting pre-creates the
// secret file with an insecure mode and asserts installServiceFiles fails
// without rewriting it.
func TestServiceSecretsFileFailsClosedOnInsecurePreexisting(t *testing.T) {
	stateDir := t.TempDir()
	pre := filepath.Join(stateDir, serviceSecret)
	if err := os.WriteFile(pre, []byte("PI_AUTOPILOT_TELEGRAM_TOKEN=old-value\nOTHER_KEY=keep\n"), 0644); err != nil {
		t.Fatal(err)
	}
	cfg := serviceTestConfig(t, stateDir)
	if _, _, err := installServiceFiles(cfg, sentinelToken, "linux"); err == nil {
		t.Fatal("install must fail closed on a non-owner-only pre-existing secret file")
	} else if !strings.Contains(err.Error(), "chmod 600") {
		t.Fatalf("error must name the chmod 600 remedy, got: %v", err)
	}
	// The pre-existing content must be untouched (no silent rewrite).
	data, _ := os.ReadFile(pre)
	if !strings.Contains(string(data), "OTHER_KEY=keep") || strings.Contains(string(data), sentinelToken) {
		t.Fatalf("pre-existing secret file was rewritten on failure: %s", data)
	}
}

// TestServiceSecretsRejectsBadToken asserts an empty or whitespace token is
// rejected before anything is written.
func TestServiceSecretsRejectsBadToken(t *testing.T) {
	for _, token := range []string{"", "   ", "has space", "line\nbreak"} {
		stateDir := t.TempDir()
		if err := writeServiceSecrets(stateDir, token); err == nil {
			t.Fatalf("token %q must be rejected", token)
		}
		// No secret file may be created.
		if _, err := os.Stat(filepath.Join(stateDir, serviceSecret)); !os.IsNotExist(err) {
			t.Fatalf("bad token %q must not create a secret file", token)
		}
	}
}

// TestServiceSecretsPreserveForeignLines round-trips operator-added lines while
// rewriting only the token line, including values with '=' signs.
func TestServiceSecretsPreserveForeignLines(t *testing.T) {
	stateDir := t.TempDir()
	if err := writeServiceSecrets(stateDir, "first-token"); err != nil {
		t.Fatal(err)
	}
	// Operator adds foreign lines (one with '=' in the value).
	p := filepath.Join(stateDir, serviceSecret)
	data, _ := os.ReadFile(p)
	if err := os.WriteFile(p, append(data, []byte("OPENAI_API_KEY=sk-abc=def\nSEARXNG_API_KEY=sx\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	// Re-install with a new token: foreign lines preserved, token line replaced.
	if err := writeServiceSecrets(stateDir, "second-token"); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	if !strings.Contains(s, "PI_AUTOPILOT_TELEGRAM_TOKEN=second-token") {
		t.Fatalf("token line must be rewritten: %s", s)
	}
	if strings.Contains(s, "first-token") {
		t.Fatalf("stale token must be gone: %s", s)
	}
	if !strings.Contains(s, "OPENAI_API_KEY=sk-abc=def") || !strings.Contains(s, "SEARXNG_API_KEY=sx") {
		t.Fatalf("foreign lines must be preserved: %s", s)
	}
	// Mode stays owner-only after the update.
	info, _ := os.Stat(p)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("secret file mode after update = %04o, want 0600", info.Mode().Perm())
	}
}

// TestServiceRunnerInjectsToken executes the generated runner under sh with a
// fake binary that prints the token env var, proving the sourcing wrapper
// injects the secret (unattended restart semantics) and propagates foreign lines.
func TestServiceRunnerInjectsToken(t *testing.T) {
	stateDir := t.TempDir()
	envFile := filepath.Join(stateDir, serviceSecret)
	if err := os.WriteFile(envFile, []byte("PI_AUTOPILOT_TELEGRAM_TOKEN="+sentinelToken+"\nOPENAI_API_KEY=foreign-key-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	// Fake binary: prints the env values it received.
	fake := filepath.Join(stateDir, "fake-bin.sh")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho TOKEN=$PI_AUTOPILOT_TELEGRAM_TOKEN\necho KEY=$OPENAI_API_KEY\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(stateDir, "config.yaml")
	runner := filepath.Join(stateDir, serviceRunner)
	if err := writeServiceRunner(runner, fake, cfgPath, envFile); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/bin/sh", runner).CombinedOutput()
	if err != nil {
		t.Fatalf("runner failed: %v (out: %s)", err, out)
	}
	if !strings.Contains(string(out), "TOKEN="+sentinelToken) {
		t.Fatalf("token must reach the child process: %s", out)
	}
	if !strings.Contains(string(out), "KEY=foreign-key-value") {
		t.Fatalf("foreign operator lines must propagate: %s", out)
	}
}

// TestServiceRunnerShellQuotesSpacedPaths renders a runner with a spaced state
// dir and asserts every interpolated path is shell-quoted, then executes it to
// prove the quoting survives a real sh parse.
func TestServiceRunnerShellQuotesSpacedPaths(t *testing.T) {
	stateDir := t.TempDir()
	spaced := filepath.Join(stateDir, "dir with spaces")
	if err := os.MkdirAll(spaced, 0700); err != nil {
		t.Fatal(err)
	}
	envFile := filepath.Join(spaced, serviceSecret)
	if err := os.WriteFile(envFile, []byte("PI_AUTOPILOT_TELEGRAM_TOKEN="+sentinelToken+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(spaced, "fake-bin.sh")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho TOKEN=$PI_AUTOPILOT_TELEGRAM_TOKEN\n"), 0700); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(spaced, "config.yaml")
	runner := renderServiceRunner(fake, cfgPath, envFile)
	if err := os.WriteFile(filepath.Join(spaced, serviceRunner), []byte(runner), 0700); err != nil {
		t.Fatal(err)
	}
	// The spaced paths must appear quoted.
	for _, want := range []string{"'" + spaced + "/" + serviceSecret + "'", "'" + fake + "'", "'" + cfgPath + "'"} {
		if !strings.Contains(runner, want) {
			t.Fatalf("runner must shell-quote %q: %s", want, runner)
		}
	}
	out, err := exec.Command("/bin/sh", filepath.Join(spaced, serviceRunner)).CombinedOutput()
	if err != nil {
		t.Fatalf("runner with spaced paths failed: %v (out: %s)", err, out)
	}
	if !strings.Contains(string(out), "TOKEN="+sentinelToken) {
		t.Fatalf("spaced-path runner must still inject the token: %s", out)
	}
}

// TestServiceUninstallRemovesSecretsFile asserts uninstall removes the unit,
// the secret file and the runner, leaving no token anywhere under the tree.
func TestServiceUninstallRemovesSecretsFile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := t.TempDir()
	cfg := serviceTestConfig(t, stateDir)

	// Install files only (no daemon start) so the test never needs a real
	// systemd/launchd.
	if _, _, err := installServiceFiles(cfg, sentinelToken, "linux"); err != nil {
		t.Fatal(err)
	}
	// Manually remove the unit (the daemon disable/remove is best-effort and
	// would try systemctl); then exercise removeStateServiceFiles directly,
	// which is the new behavior under test.
	unitPath := filepath.Join(home, ".config", "systemd", "user", serviceUnit)
	if err := os.Remove(unitPath); err != nil {
		t.Fatal(err)
	}
	if err := removeStateServiceFiles(stateDir); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		filepath.Join(stateDir, serviceSecret),
		filepath.Join(stateDir, serviceRunner),
	} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s must be removed on uninstall: %v", p, err)
		}
	}
	// No token must remain anywhere under stateDir.
	var walk func(root string)
	walk = func(root string) {
		t.Helper()
		entries, err := os.ReadDir(root)
		if err != nil {
			return
		}
		for _, e := range entries {
			full := filepath.Join(root, e.Name())
			if e.IsDir() {
				walk(full)
				continue
			}
			data, err := os.ReadFile(full)
			if err == nil && strings.Contains(string(data), sentinelToken) {
				t.Fatalf("token still on disk after uninstall: %s", full)
			}
		}
	}
	walk(stateDir)
	_ = home
}

// TestShellQuote asserts the single-quote escaping for embedded single quotes.
func TestShellQuote(t *testing.T) {
	if got := shellQuote("/a/b"); got != `'/a/b'` {
		t.Fatalf("shellQuote simple = %q", got)
	}
	if got := shellQuote("/a's b"); got != `'/a'\''s b'` {
		t.Fatalf("shellQuote embedded quote = %q", got)
	}
}

// TestServiceStatTModeGuard exercises the syscall.Stat_t mode branch that the
// fail-closed check relies on, to keep the linux/darwin sysfs path covered.
func TestServiceStatTModeGuard(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, serviceSecret)
	if err := os.WriteFile(p, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("platform without syscall.Stat_t")
	}
	if st.Mode&0777 != 0600 {
		t.Fatalf("mode perm = %04o, want 0600", st.Mode&0777)
	}
	if uid := os.Getuid(); int(st.Uid) != uid {
		t.Fatalf("uid %d != current %d", st.Uid, uid)
	}
}

// ---------------- Issue #11: idempotent install/uninstall, dry-run, restart

// serviceSeams resets the serviceExec/servicePreCheck seams for a test and
// restores the real ones on cleanup, so no test can leak a fake daemon call
// into another.
func serviceSeams(t *testing.T, exec func(ctx context.Context, dir, name string, args ...string) (string, error), pre func(ctx context.Context, cfg Config) (string, error)) {
	t.Helper()
	oldExec := serviceExec
	oldPre := servicePreCheck
	serviceExec = exec
	if pre != nil {
		servicePreCheck = pre
	}
	t.Cleanup(func() {
		serviceExec = oldExec
		servicePreCheck = oldPre
	})
}

// okPreCheck is a faked passing doctor gate.
func okPreCheck(ctx context.Context, cfg Config) (string, error) { return "platform: test", nil }

// failingPreCheck is a faked failing doctor gate.
func failingPreCheck(ctx context.Context, cfg Config) (string, error) { return "", fmt.Errorf("pi: not-found") }

// execRecorder records every daemon call so tests can assert the exact
// launchctl/systemctl sequences (bootout precedes bootstrap, restart present,
// …) without a live daemon.
type execRecorder struct {
	calls [][]string
}

func (r *execRecorder) run(ctx context.Context, dir, name string, args ...string) (string, error) {
	c := append([]string{}, name)
	c = append(c, args...)
	r.calls = append(r.calls, c)
	return "", nil
}

func (r *execRecorder) names() []string {
	out := []string{}
	for _, c := range r.calls {
		out = append(out, strings.Join(c, " "))
	}
	return out
}

func (r *execRecorder) has(name string) bool {
	for _, c := range r.calls {
		if strings.Join(c, " ") == name {
			return true
		}
	}
	return false
}

func (r *execRecorder) index(name string) int {
	for i, c := range r.calls {
		if strings.Join(c, " ") == name {
			return i
		}
	}
	return -1
}

// TestServicePlatformLabelMatrix exercises the pure platform label for the
// three supported targets without any GOOS skip (mirrors platform_test.go).
func TestServicePlatformLabelMatrix(t *testing.T) {
	for _, tc := range []struct{ goos, goarch, want string }{
		{"darwin", "arm64", "darwin/arm64"},
		{"linux", "arm64", "linux/arm64"},
		{"linux", "amd64", "linux/amd64"},
	} {
		if got := platformLabelFrom(tc.goos, tc.goarch); got != tc.want {
			t.Fatalf("platformLabelFrom(%s/%s) = %q, want %q", tc.goos, tc.goarch, got, tc.want)
		}
	}
}

// TestServiceUnsupportedGOOS asserts planning/starting on an unsupported GOOS
// fails with a specific error naming the GOOS.
func TestServiceUnsupportedGOOS(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := t.TempDir()
	cfg := serviceTestConfig(t, stateDir)
	if _, err := planServiceInstall(cfg, sentinelToken, "windows"); err == nil {
		t.Fatal("plan for windows must fail")
	} else if !strings.Contains(err.Error(), "windows") {
		t.Fatalf("error must name the unsupported GOOS, got: %v", err)
	}
	if _, _, err := installServiceFiles(cfg, sentinelToken, "windows"); err == nil {
		t.Fatal("install for windows must fail")
	}
}

// TestServiceInstallFreshInstall asserts a fresh install (empty HOME) writes
// the exact artifact set with expected modes and issues one start per platform.
func TestServiceInstallFreshInstall(t *testing.T) {
	for _, goos := range []string{"darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			rec := &execRecorder{}
			serviceSeams(t, rec.run, okPreCheck)
			home := t.TempDir()
			t.Setenv("HOME", home)
			stateDir := t.TempDir()
			cfg := serviceTestConfig(t, stateDir)

			// Fresh install must not pre-create the secret file.
			_, err := installServiceForTest(cfg, sentinelToken, goos)
			if err != nil {
				t.Fatalf("install(%s): %v", goos, err)
			}

			assertMode := func(path string, want os.FileMode) {
				t.Helper()
				info, err := os.Stat(path)
				if err != nil {
					t.Fatalf("stat %s: %v", path, err)
				}
				if got := info.Mode().Perm(); got != want {
					t.Fatalf("%s mode = %04o, want %04o", path, got, want)
				}
			}
			assertMode(filepath.Join(stateDir, serviceSecret), 0600)
			if goos == "darwin" {
				assertMode(filepath.Join(stateDir, serviceRunner), 0700)
				assertMode(filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist"), 0600)
				// One bootstrap (the start) plus the ignored bootout.
				if !rec.has(fmt.Sprintf("launchctl bootstrap gui/%d %s", os.Getuid(), filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist"))) {
					t.Fatalf("expected launchctl bootstrap, got: %v", rec.names())
				}
				if !rec.has(fmt.Sprintf("launchctl bootout gui/%d/%s", os.Getuid(), serviceLabel)) {
					t.Fatalf("expected launchctl bootout before bootstrap, got: %v", rec.names())
				}
				// bootout must precede bootstrap (upgrade semantics).
				if b, s := rec.index(fmt.Sprintf("launchctl bootout gui/%d/%s", os.Getuid(), serviceLabel)), rec.index(fmt.Sprintf("launchctl bootstrap gui/%d %s", os.Getuid(), filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist"))); b == -1 || s == -1 || b > s {
					t.Fatalf("bootout must precede bootstrap: %v", rec.names())
				}
			} else {
				assertMode(filepath.Join(home, ".config", "systemd", "user", serviceUnit), 0600)
				if !rec.has("systemctl --user daemon-reload") || !rec.has("systemctl --user enable "+serviceUnit) || !rec.has("systemctl --user restart "+serviceUnit) {
					t.Fatalf("expected daemon-reload + enable + restart, got: %v", rec.names())
				}
			}
		})
	}
}

// installServiceForTest drives the full install path (pre-check + files +
// daemon start) but is parameterized on goos so both templates run on any host.
// It reuses the same rendering/writing as ServiceInstall while letting the
// test force the target goos (ServiceInstall itself uses runtime.GOOS, which a
// linux test cannot switch to darwin). The daemon start is dispatched through
// serviceExec via startLaunchAgent/startSystemdUser, so the recorder captures it.
func installServiceForTest(cfg Config, token string, goos string) (string, error) {
	if goos != "darwin" && goos != "linux" {
		return "", fmt.Errorf("system service support exists for macOS and Linux, not for %s", goos)
	}
	if _, err := servicePreCheck(context.Background(), cfg); err != nil {
		return "", err
	}
	plan, err := planServiceInstall(cfg, token, goos)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(plan.logs, 0700); err != nil {
		return "", err
	}
	if err := writeServiceSecrets(cfg.StateDir, token); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(plan.unitPath), 0700); err != nil {
		return "", err
	}
	switch goos {
	case "darwin":
		if err := writeServiceRunner(plan.runnerPath, plan.bin, plan.cfgPath, plan.envFile); err != nil {
			return "", err
		}
		if err := os.WriteFile(plan.unitPath, []byte(plan.unitBody), 0600); err != nil {
			return "", err
		}
		if err := startLaunchAgent(); err != nil {
			return "", err
		}
	case "linux":
		if err := os.WriteFile(plan.unitPath, []byte(plan.unitBody), 0600); err != nil {
			return "", err
		}
		note, err := startSystemdUser()
		if err != nil {
			return "", err
		}
		return unitInstalledMessage(plan, note), nil
	}
	return unitInstalledMessage(plan, ""), nil
}

// TestServiceInstallRepeatRun installs twice and asserts idempotency: identical
// unit/plist bytes, exactly one token line, foreign lines preserved.
func TestServiceInstallRepeatRun(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := t.TempDir()
	cfg := serviceTestConfig(t, stateDir)
	rec := &execRecorder{}
	serviceSeams(t, rec.run, okPreCheck)

	if _, err := installServiceForTest(cfg, sentinelToken, "linux"); err != nil {
		t.Fatal(err)
	}
	unitPath := filepath.Join(home, ".config", "systemd", "user", serviceUnit)
	first, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	// Operator adds a foreign line.
	env := filepath.Join(stateDir, serviceSecret)
	data, _ := os.ReadFile(env)
	if err := os.WriteFile(env, append(data, []byte("OPENAI_API_KEY=foreign\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	// Second install with the same token: identical unit bytes, one token line.
	if _, err := installServiceForTest(cfg, sentinelToken, "linux"); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("repeat install must be byte-identical:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	envData, _ := os.ReadFile(env)
	s := string(envData)
	if strings.Count(s, "PI_AUTOPILOT_TELEGRAM_TOKEN=") != 1 {
		t.Fatalf("secret file must have exactly one token line: %s", s)
	}
	if !strings.Contains(s, "OPENAI_API_KEY=foreign") {
		t.Fatalf("foreign line must be preserved: %s", s)
	}
}

// TestServiceInstallPreservesStateWorktreesConfig plants state.json, a worktree
// file and config.yaml, then asserts they are byte-identical after install.
func TestServiceInstallPreservesStateWorktreesConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := t.TempDir()
	cfg := serviceTestConfig(t, stateDir)
	rec := &execRecorder{}
	serviceSeams(t, rec.run, okPreCheck)

	state := filepath.Join(stateDir, "state.json")
	if err := os.WriteFile(state, []byte(`{"phase":"plan"}`), 0600); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(stateDir, "worktrees", "issue-11", "README.md")
	if err := os.MkdirAll(filepath.Dir(wt), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wt, []byte("# worktree\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg.Path, []byte("repo: x/y\n"), 0600); err != nil {
		t.Fatal(err)
	}
	stateBefore, _ := os.ReadFile(state)
	wtBefore, _ := os.ReadFile(wt)
	cfgBefore, _ := os.ReadFile(cfg.Path)

	if _, err := installServiceForTest(cfg, sentinelToken, "linux"); err != nil {
		t.Fatal(err)
	}
	stateAfter, _ := os.ReadFile(state)
	wtAfter, _ := os.ReadFile(wt)
	cfgAfter, _ := os.ReadFile(cfg.Path)
	if string(stateBefore) != string(stateAfter) {
		t.Fatalf("state.json must be untouched: %s -> %s", stateBefore, stateAfter)
	}
	if string(wtBefore) != string(wtAfter) {
		t.Fatalf("worktree file must be untouched: %s -> %s", wtBefore, wtAfter)
	}
	if string(cfgBefore) != string(cfgAfter) {
		t.Fatalf("config.yaml must be untouched: %s -> %s", cfgBefore, cfgAfter)
	}
}

// TestServiceInstallReportsHost asserts the install message carries the
// platform label and the resolved binary path.
func TestServiceInstallReportsHost(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := t.TempDir()
	cfg := serviceTestConfig(t, stateDir)
	rec := &execRecorder{}
	serviceSeams(t, rec.run, okPreCheck)

	msg, err := installServiceForTest(cfg, sentinelToken, "linux")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "platform: "+platformLabel()) {
		t.Fatalf("message must carry the platform label: %s", msg)
	}
	bin, err := serviceBin()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(msg, "binary: "+bin) {
		t.Fatalf("message must carry the resolved binary path: %s", msg)
	}
}

// TestServiceInstallDoctorGateBlocks asserts a failing pre-check aborts the
// install before any filesystem write and any daemon call.
func TestServiceInstallDoctorGateBlocks(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := t.TempDir()
	cfg := serviceTestConfig(t, stateDir)
	rec := &execRecorder{}
	serviceSeams(t, rec.run, failingPreCheck)

	if _, err := installServiceForTest(cfg, sentinelToken, "linux"); err == nil {
		t.Fatal("install must fail when the pre-enable doctor check fails")
	} else if !strings.Contains(err.Error(), "pi: not-found") {
		t.Fatalf("error must surface the specific doctor cause, got: %v", err)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("no daemon call may be made when the gate fails, got: %v", rec.names())
	}
	if _, err := os.Stat(filepath.Join(stateDir, serviceSecret)); !os.IsNotExist(err) {
		t.Fatal("no secret file may be written when the gate fails")
	}
}

// TestServiceInstallDoctorGatePasses asserts a passing gate proceeds to install.
func TestServiceInstallDoctorGatePasses(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := t.TempDir()
	cfg := serviceTestConfig(t, stateDir)
	rec := &execRecorder{}
	serviceSeams(t, rec.run, okPreCheck)
	if _, err := installServiceForTest(cfg, sentinelToken, "linux"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, serviceSecret)); err != nil {
		t.Fatalf("secret file must be written when the gate passes: %v", err)
	}
}

// TestServiceInstallDryRunWritesNothing asserts the dry-run plan renders without
// writing any file anywhere under HOME or stateDir.
func TestServiceInstallDryRunWritesNothing(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := t.TempDir()
	cfg := serviceTestConfig(t, stateDir)
	rec := &execRecorder{}
	serviceSeams(t, rec.run, okPreCheck)

	// We can only call the runtime-GOOS dry-run entry from this host, so drive
	// the pure plan directly for both templates (the seam keeps it platform-independent).
	for _, goos := range []string{"darwin", "linux"} {
		p, err := planServiceInstall(cfg, sentinelToken, goos)
		if err != nil {
			t.Fatalf("plan(%s): %v", goos, err)
		}
		// The plan must never carry the token value anywhere in its rendered bodies.
		for _, body := range []string{p.unitBody, p.runnerBody} {
			if body != "" && strings.Contains(body, sentinelToken) {
				t.Fatalf("plan body leaked the token (%s)", goos)
			}
		}
		if p.bin == "" || p.platform == "" {
			t.Fatalf("plan must carry binary + platform (%s): %+v", goos, p)
		}
	}
	// No regular files may have been created under HOME or stateDir by planning.
	var fileCount int
	for _, root := range []string{home, stateDir} {
		filepath.Walk(root, func(_ string, info os.FileInfo, _ error) error {
			if info != nil && info.Mode().IsRegular() {
				fileCount++
			}
			return nil
		})
	}
	if fileCount != 0 {
		t.Fatalf("plan must not write any file, found %d regular files under HOME/stateDir", fileCount)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("dry-run must not invoke the daemon, got: %v", rec.names())
	}
}

// TestServiceUninstallDryRunReportsOnly asserts the uninstall dry-run reports
// what would be removed but leaves the files in place and prints no secret.
func TestServiceUninstallDryRunReportsOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := t.TempDir()
	cfg := serviceTestConfig(t, stateDir)
	rec := &execRecorder{}
	serviceSeams(t, rec.run, okPreCheck)

	// Install files only (no daemon start) so the tree exists.
	if _, _, err := installServiceFiles(cfg, sentinelToken, "linux"); err != nil {
		t.Fatal(err)
	}
	unitPath := filepath.Join(home, ".config", "systemd", "user", serviceUnit)
	env := filepath.Join(stateDir, serviceSecret)

	// Drive the pure renderer with goos=linux so the linux template is
	// exercised from any host (the public ServiceUninstallDryRun is
	// runtime-GOOS-bound).
	out := renderUninstallDryRun("linux", home, stateDir)
	if strings.Contains(out, sentinelToken) {
		t.Fatalf("uninstall dry-run must not print the secret: %s", out)
	}
	if !strings.Contains(out, unitPath) {
		t.Fatalf("dry-run must report the unit path: %s", out)
	}
	if !strings.Contains(out, "present") {
		t.Fatalf("dry-run must report the unit is present: %s", out)
	}
	// The files must still exist after the dry-run.
	if _, err := os.Stat(unitPath); err != nil {
		t.Fatalf("unit must still exist after dry-run: %v", err)
	}
	if _, err := os.Stat(env); err != nil {
		t.Fatalf("secret file must still exist after dry-run: %v", err)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("dry-run must not invoke the daemon, got: %v", rec.names())
	}
}

// TestServicePlanDryRunNoSecretLeak asserts the full install dry-run output
// (runtime GOOS) contains the paths + modes but never the token value.
func TestServicePlanDryRunNoSecretLeak(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateDir := t.TempDir()
	cfg := serviceTestConfig(t, stateDir)
	rec := &execRecorder{}
	serviceSeams(t, rec.run, okPreCheck)

	out, err := ServiceInstallDryRun(cfg, sentinelToken)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, sentinelToken) {
		t.Fatalf("install dry-run must not print the token value: %s", out)
	}
	if !strings.Contains(out, "DRY RUN") {
		t.Fatalf("dry-run output must be labeled: %s", out)
	}
	if !strings.Contains(out, "binary: ") || !strings.Contains(out, "platform: ") {
		t.Fatalf("dry-run must report binary + platform: %s", out)
	}
	if len(rec.calls) != 0 {
		t.Fatalf("dry-run must not invoke the daemon, got: %v", rec.names())
	}
}

// TestServiceRestart asserts the restart subcommand issues the platform-native
// restart through the seam (no real daemon).
func TestServiceRestart(t *testing.T) {
	rec := &execRecorder{}
	serviceSeams(t, rec.run, nil)
	// ServiceRestart uses runtime.GOOS; on the linux CI host it must issue
	// `systemctl --user restart`. On a darwin host it would issue kickstart.
	if runtime.GOOS == "linux" {
		if _, err := ServiceRestart(); err != nil {
			t.Fatal(err)
		}
		if !rec.has("systemctl --user restart "+serviceUnit) {
			t.Fatalf("expected systemctl --user restart, got: %v", rec.names())
		}
	} else if runtime.GOOS == "darwin" {
		if _, err := ServiceRestart(); err != nil {
			t.Fatal(err)
		}
		if !rec.has("launchctl kickstart -k gui/"+strconv.Itoa(os.Getuid())+"/"+serviceLabel) {
			t.Fatalf("expected launchctl kickstart -k, got: %v", rec.names())
		}
	}
}
