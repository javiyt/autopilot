package autopilot

import (
	"os"
	"os/exec"
	"path/filepath"
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
