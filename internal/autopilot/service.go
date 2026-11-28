package autopilot

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// A user-level service keeps the supervisor running across terminal sessions
// and restarts it on exit: a launchd LaunchAgent on macOS and a systemd user
// unit on Linux. Nothing here requires root.
//
// Secret handling (issue #10): the Telegram token never appears in the unit
// definition. It is written once to an owner-only secret env file
// (<state_dir>/service-secrets.env, mode 0600) that the service references:
//   - Linux systemd: EnvironmentFile=<state_dir>/service-secrets.env
//   - macOS launchd: ProgramArguments = ["/bin/sh", <state_dir>/service-run.sh]
//     where the runner (mode 0700) sources the env file and exec's the binary.
//
// The same env file is mount-friendly for containers (Podman secret /
// bind-mount), so one portable secret model covers native + container.

const (
	serviceLabel  = "com.javi.pi-autopilot"
	serviceUnit   = "pi-autopilot.service"
	serviceSecret = "service-secrets.env"
	serviceRunner = "service-run.sh"
)

func serviceBin() (string, error) {
	bin, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(bin)
}

// ServiceInstall writes the platform unit plus the owner-only secret file and
// (macOS) the sourcing runner, then starts the daemon. The token is stored in
// the secret file, never in the unit, so the service runs without an
// interactive shell exporting it.
func ServiceInstall(cfg Config, token string) (string, error) {
	_, message, err := installServiceFiles(cfg, token, runtime.GOOS)
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "darwin":
		if err := startLaunchAgent(); err != nil {
			return "", err
		}
	case "linux":
		note, err := startSystemdUser()
		if err != nil {
			return "", err
		}
		message += note
	default:
		return "", fmt.Errorf("system service support exists for macOS and Linux, not for %s", runtime.GOOS)
	}
	return message, nil
}

// installServiceFiles renders and writes the unit, the owner-only secret env
// file and (macOS) the runner script, asserting their permissions. It is a
// pure filesystem seam: no daemon start, and the goos parameter means both
// templates can be exercised from any host (this is what keeps the tests
// platform-independent). Returns the unit path, a human message, and an error.
func installServiceFiles(cfg Config, token string, goos string) (string, string, error) {
	bin, err := serviceBin()
	if err != nil {
		return "", "", fmt.Errorf("resolve binary: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	cfgPath, err := filepath.Abs(cfg.Path)
	if err != nil {
		return "", "", err
	}
	logs := filepath.Join(cfg.StateDir, "service-logs")
	if err := os.MkdirAll(logs, 0700); err != nil {
		return "", "", err
	}
	// Write the owner-only secret file first: a failure here fails the whole
	// install before the unit references a file that does not exist.
	if err := writeServiceSecrets(cfg.StateDir, token); err != nil {
		return "", "", err
	}
	envFile := filepath.Join(cfg.StateDir, serviceSecret)
	switch goos {
	case "darwin":
		dir := filepath.Join(home, "Library", "LaunchAgents")
		if err := os.MkdirAll(dir, 0700); err != nil {
			return "", "", err
		}
		runner := filepath.Join(cfg.StateDir, serviceRunner)
		if err := writeServiceRunner(runner, bin, cfgPath, envFile); err != nil {
			return "", "", err
		}
		path := filepath.Join(dir, serviceLabel+".plist")
		plist := renderLaunchAgentPlist(cfgPath, runner, os.Getenv("PATH"), logs)
		if err := os.WriteFile(path, []byte(plist), 0600); err != nil {
			return "", "", fmt.Errorf("write LaunchAgent: %w", err)
		}
		return path, "LaunchAgent installed at " + path + ". Secrets in " + envFile + ". Logs: " + logs + ". Remove it with: pi-autopilot service uninstall", nil
	case "linux":
		dir := filepath.Join(home, ".config", "systemd", "user")
		if err := os.MkdirAll(dir, 0700); err != nil {
			return "", "", err
		}
		unitPath := filepath.Join(dir, serviceUnit)
		unit := renderSystemdUnit(bin, cfgPath, envFile, os.Getenv("PATH"), logs)
		if err := os.WriteFile(unitPath, []byte(unit), 0600); err != nil {
			return "", "", fmt.Errorf("write systemd unit: %w", err)
		}
		return unitPath, "systemd user unit installed at " + unitPath + ". Secrets in " + envFile + ". Remove it with: pi-autopilot service uninstall.", nil
	default:
		return "", "", fmt.Errorf("system service support exists for macOS and Linux, not for %s", goos)
	}
}

// writeServiceSecrets creates or updates <stateDir>/service-secrets.env at
// mode 0600, preserving any operator-added lines while rewriting only the
// PI_AUTOPILOT_TELEGRAM_TOKEN= line. Fail-closed: an empty or whitespace
// token is rejected, and a pre-existing file with a non-0600 mode or a
// non-matching owner uid is rejected (with the exact remedy) rather than
// auto-rewritten.
func writeServiceSecrets(stateDir, token string) error {
	if strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n \t") {
		return fmt.Errorf("telegram token must be a single non-empty value without whitespace (it came in empty or with CR/LF/whitespace; fix the PI_AUTOPILOT_TELEGRAM_TOKEN export)")
	}
	path := filepath.Join(stateDir, serviceSecret)
	// Pre-existing file: enforce owner-only perms + owner before touching it.
	if info, err := os.Stat(path); err == nil {
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			return fmt.Errorf("cannot stat %s for permission check", path)
		}
		if perm := st.Mode & 0777; perm != 0600 {
			return fmt.Errorf("%s is not owner-only (mode %04o); fix with: chmod 600 %s", path, perm, path)
		}
		if uid := os.Getuid(); int(st.Uid) != uid {
			return fmt.Errorf("%s is owned by uid %d, not the current user (%d); fix with: chown %d %s", path, st.Uid, uid, uid, path)
		}
	}
	// Build the new content: preserve foreign lines, replace the token line.
	lines := []string{}
	if data, err := os.ReadFile(path); err == nil {
		for _, ln := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
			if strings.HasPrefix(ln, "PI_AUTOPILOT_TELEGRAM_TOKEN=") || strings.TrimSpace(ln) == "" {
				continue // drop stale token line / blank; token re-added below
			}
			lines = append(lines, ln)
		}
	}
	content := "PI_AUTOPILOT_TELEGRAM_TOKEN=" + token + "\n"
	for _, ln := range lines {
		content += ln + "\n"
	}
	if err := os.MkdirAll(stateDir, 0700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	// Re-stat: guard against umask/overlay surprises that relaxed the mode.
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		return fmt.Errorf("%s written but mode is %04o, expected 0600; fix with: chmod 600 %s", path, perm, path)
	}
	return nil
}

// writeServiceRunner writes the macOS wrapper script (mode 0700) that sources
// the secret env file and exec's the binary. Every interpolated path is
// shell-quoted so spaces in state_dir are safe.
func writeServiceRunner(runnerPath, bin, cfgPath, envFile string) error {
	script := renderServiceRunner(bin, cfgPath, envFile)
	if err := os.WriteFile(runnerPath, []byte(script), 0700); err != nil {
		return fmt.Errorf("write %s: %w", runnerPath, err)
	}
	info, err := os.Stat(runnerPath)
	if err != nil {
		return err
	}
	if perm := info.Mode().Perm(); perm != 0700 {
		return fmt.Errorf("%s written but mode is %04o, expected 0700; fix with: chmod 700 %s", runnerPath, perm, runnerPath)
	}
	return nil
}

// renderServiceRunner is the pure template for the macOS wrapper: it sources
// the owner-only env file (set -a exports every KEY=VALUE, including any
// operator-added provider keys) and exec's the binary with the config.
func renderServiceRunner(bin, cfgPath, envFile string) string {
	quote := shellQuote
	return `#!/bin/sh
set -a
. ` + quote(envFile) + `
set +a
exec ` + quote(bin) + ` run -config ` + quote(cfgPath) + `
`
}

// renderLaunchAgentPlist is the pure macOS template: ProgramArguments runs the
// sourcing wrapper, and only PATH is carried in EnvironmentVariables. No secret.
func renderLaunchAgentPlist(cfgPath, runnerPath, pathEnv, logsDir string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + serviceLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>/bin/sh</string>
		<string>` + runnerPath + `</string>
	</array>
	<key>WorkingDirectory</key>
	<string>` + filepath.Dir(cfgPath) + `</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>` + pathEnv + `</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>` + filepath.Join(logsDir, "service.out.log") + `</string>
	<key>StandardErrorPath</key>
	<string>` + filepath.Join(logsDir, "service.err.log") + `</string>
</dict>
</plist>
`
}

// renderSystemdUnit is the pure Linux template: the secret is pulled from the
// owner-only env file via a hard EnvironmentFile= (no leading '-' so a missing
// file fails the unit instead of starting without the token).
func renderSystemdUnit(bin, cfgPath, envFile, pathEnv, logsDir string) string {
	return fmt.Sprintf(`[Unit]
Description=Pi Autopilot (GitHub delivery supervisor)
After=network-online.target

[Service]
WorkingDirectory=%s
ExecStart=%s run -config %s
Environment=PATH=%s
EnvironmentFile=%s
StandardOutput=append:%s
StandardError=append:%s
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
`, filepath.Dir(cfgPath), bin, cfgPath, pathEnv, envFile, filepath.Join(logsDir, "service.out.log"), filepath.Join(logsDir, "service.err.log"))
}

// shellQuote wraps a value in single quotes for POSIX sh, escaping embedded
// single quotes as '\''.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func startLaunchAgent() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	path := filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist")
	uid := strconv.Itoa(os.Getuid())
	// Modern bootstrap first; older launchctl only knows load.
	if _, err := command(context.Background(), "", "launchctl", "bootstrap", "gui/"+uid, path); err != nil {
		if _, err := command(context.Background(), "", "launchctl", "load", "-w", path); err != nil {
			return fmt.Errorf("LaunchAgent written to %s but launchctl could not start it: %v (try: launchctl bootstrap gui/%s %s)", path, err, uid, path)
		}
	}
	return nil
}

// startSystemdUser reloads and enables the unit, returning an optional note
// (e.g. linger advice) to be appended to the install message.
func startSystemdUser() (string, error) {
	if _, err := command(context.Background(), "", "systemctl", "--user", "daemon-reload"); err != nil {
		return "", fmt.Errorf("systemd daemon-reload: %w", err)
	}
	if out, err := command(context.Background(), "", "systemctl", "--user", "enable", "--now", serviceUnit); err != nil {
		return "", fmt.Errorf("systemd enable --now: %v (out: %s)", err, out)
	}
	note := ""
	if u, err := user.Current(); err == nil {
		if linger, _ := command(context.Background(), "", "loginctl", "show-user", u.Username, "-p", "Linger"); strings.TrimSpace(linger) != "Linger=yes" {
			note = "\nNote: the unit runs while you are logged in; run 'loginctl enable-linger' for boot start without a login session."
		}
	}
	return note, nil
}

// ServiceUninstall removes the unit and (when a state_dir is known) the
// owner-only secret file and runner script, so no token lingers on disk after
// removal. cfg may be a zero value: with an empty StateDir only the fixed-path
// unit is removed, keeping uninstall robust when the config file is gone.
func ServiceUninstall(cfg Config) (string, error) {
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path := filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist")
		uid := strconv.Itoa(os.Getuid())
		_, _ = command(context.Background(), "", "launchctl", "bootout", "gui/"+uid+"/"+serviceLabel)
		_, _ = command(context.Background(), "", "launchctl", "unload", "-w", path)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return "", err
		}
		msg := "LaunchAgent removed from " + path
		if cfg.StateDir != "" {
			if err := removeStateServiceFiles(cfg.StateDir); err != nil {
				return "", err
			}
			msg += "; removed " + serviceSecret + " and " + serviceRunner + " from " + cfg.StateDir
		}
		return msg, nil
	case "linux":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		unitPath := filepath.Join(home, ".config", "systemd", "user", serviceUnit)
		_, _ = command(context.Background(), "", "systemctl", "--user", "disable", "--now", serviceUnit)
		_, _ = command(context.Background(), "", "systemctl", "--user", "daemon-reload")
		if err := os.Remove(unitPath); err != nil && !os.IsNotExist(err) {
			return "", err
		}
		msg := "systemd user unit removed from " + unitPath
		if cfg.StateDir != "" {
			if err := removeStateServiceFiles(cfg.StateDir); err != nil {
				return "", err
			}
			msg += "; removed " + serviceSecret + " from " + cfg.StateDir
		}
		return msg, nil
	default:
		return "", fmt.Errorf("system service support exists for macOS and Linux, not for %s", runtime.GOOS)
	}
}

// removeStateServiceFiles deletes the owner-only secret file and runner script
// under stateDir (best-effort: absence is fine).
func removeStateServiceFiles(stateDir string) error {
	for _, name := range []string{serviceSecret, serviceRunner} {
		p := filepath.Join(stateDir, name)
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func ServiceStart() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		uid := strconv.Itoa(os.Getuid())
		if _, err := command(context.Background(), "", "launchctl", "kickstart", "-k", "gui/"+uid+"/"+serviceLabel); err != nil {
			return "", fmt.Errorf("launchctl kickstart: %w (is the LaunchAgent installed? pi-autopilot service install)", err)
		}
		return "service started", nil
	case "linux":
		if _, err := command(context.Background(), "", "systemctl", "--user", "start", serviceUnit); err != nil {
			return "", fmt.Errorf("systemctl --user start: %w (installed? pi-autopilot service install)", err)
		}
		return "service started", nil
	default:
		return "", fmt.Errorf("system service support exists for macOS and Linux, not for %s", runtime.GOOS)
	}
}

func ServiceStop() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		if _, err := command(context.Background(), "", "launchctl", "stop", serviceLabel); err != nil {
			return "", fmt.Errorf("launchctl stop: %w", err)
		}
		return "service stopped", nil
	case "linux":
		if _, err := command(context.Background(), "", "systemctl", "--user", "stop", serviceUnit); err != nil {
			return "", fmt.Errorf("systemctl --user stop: %w", err)
		}
		return "service stopped", nil
	default:
		return "", fmt.Errorf("system service support exists for macOS and Linux, not for %s", runtime.GOOS)
	}
}

func ServiceStatus() (string, error) {
	switch runtime.GOOS {
	case "darwin":
		out, err := command(context.Background(), "", "launchctl", "list")
		if err != nil {
			return "", err
		}
		for _, line := range strings.Split(out, "\n") {
			if strings.Contains(line, serviceLabel) {
				return "LaunchAgent " + serviceLabel + " is loaded:\n" + line, nil
			}
		}
		return "LaunchAgent " + serviceLabel + " is not loaded. Install it with: pi-autopilot service install", nil
	case "linux":
		out, err := command(context.Background(), "", "systemctl", "--user", "show", "-p", "ActiveState,SubState,MainPID", serviceUnit)
		if err != nil {
			return "", fmt.Errorf("systemctl --user show: %w (installed? pi-autopilot service install)", err)
		}
		return "systemd user unit " + serviceUnit + ":\n" + out, nil
	default:
		return "", fmt.Errorf("system service support exists for macOS and Linux, not for %s", runtime.GOOS)
	}
}
