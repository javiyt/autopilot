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
//
// Issue #11: install is idempotent (bootout-before-bootstrap / unconditional
// systemd restart), gated by a fail-closed pre-enable doctor check, reports
// the host platform + binary path, supports dry-run, and adds `service
// restart`. Every daemon call goes through the serviceExec seam so the smoke
// tests never need a live launchctl/systemd user session.

const (
	serviceLabel  = "com.javi.pi-autopilot"
	serviceUnit   = "pi-autopilot.service"
	serviceSecret = "service-secrets.env"
	serviceRunner = "service-run.sh"
)

// serviceExec is the daemon-command seam: production runs the real command(),
// tests swap in a recorder so no smoke test ever needs a live launchctl or
// systemd user session (GHA ubuntu runners have no usable one). Every
// launchctl/systemctl/loginctl call in this file goes through it.
var serviceExec = command

// servicePreCheck is the pre-enable gate for `service install`: the full
// doctor (git/gh/pi presence, gh auth, managed Pi + models, key sources,
// remote endpoints). Fail-closed: any error aborts the install before a
// single byte is written and before any daemon call.
var servicePreCheck = Doctor

func serviceBin() (string, error) {
	bin, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(bin)
}

// serviceInstallPlan is the pure rendering of everything `service install`
// would touch: the unit/plist/runner paths and bodies, the owner-only secret
// file path and its disposition (create or update — decided from existence
// only, the file's content is never read or returned), and the log directory.
// One source of truth for both the real install and the dry-run report.
type serviceInstallPlan struct {
	goos              string
	platform          string // "<os>/<arch>" host label
	bin               string // resolved executable path the unit will point at
	cfgPath           string
	logs              string
	unitPath          string // LaunchAgent plist on darwin, systemd unit on linux
	unitBody          string // rendered plist / unit text
	runnerPath        string // darwin only
	runnerBody        string // darwin only
	envFile           string // owner-only secret env file
	secretDisposition string // "create" | "update"
}

// planServiceInstall renders the install plan without touching the filesystem
// (it stats the secret file only to decide create-vs-update). The goos
// parameter mirrors installServiceFiles: both templates can be planned from
// any host, which keeps the tests platform-independent. Unsupported GOOS fails
// with a specific error, and an invalid token is rejected before anything
// else.
func planServiceInstall(cfg Config, token string, goos string) (serviceInstallPlan, error) {
	var p serviceInstallPlan
	if strings.TrimSpace(token) == "" || strings.ContainsAny(token, "\r\n \t") {
		return p, fmt.Errorf("telegram token must be a single non-empty value without whitespace (it came in empty or with CR/LF/whitespace; fix the PI_AUTOPILOT_TELEGRAM_TOKEN export)")
	}
	bin, err := serviceBin()
	if err != nil {
		return p, fmt.Errorf("resolve binary: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p, err
	}
	cfgPath, err := filepath.Abs(cfg.Path)
	if err != nil {
		return p, err
	}
	p.goos = goos
	p.platform = platformLabel()
	p.bin = bin
	p.cfgPath = cfgPath
	p.logs = filepath.Join(cfg.StateDir, "service-logs")
	p.envFile = filepath.Join(cfg.StateDir, serviceSecret)
	if _, err := os.Stat(p.envFile); err == nil {
		p.secretDisposition = "update"
	} else {
		p.secretDisposition = "create"
	}
	switch goos {
	case "darwin":
		p.unitPath = filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist")
		p.runnerPath = filepath.Join(cfg.StateDir, serviceRunner)
		p.unitBody = renderLaunchAgentPlist(cfgPath, p.runnerPath, os.Getenv("PATH"), p.logs)
		p.runnerBody = renderServiceRunner(bin, cfgPath, p.envFile)
	case "linux":
		p.unitPath = filepath.Join(home, ".config", "systemd", "user", serviceUnit)
		p.unitBody = renderSystemdUnit(bin, cfgPath, p.envFile, os.Getenv("PATH"), p.logs)
	default:
		return p, fmt.Errorf("system service support exists for macOS and Linux, not for %s", goos)
	}
	return p, nil
}

// unitInstalledMessage renders the post-install summary: platform, the
// resolved binary path the unit points at, and the artifact paths.
func unitInstalledMessage(p serviceInstallPlan, note string) string {
	var b strings.Builder
	b.WriteString("platform: " + p.platform + "\n")
	b.WriteString("binary: " + p.bin + "\n")
	if p.goos == "darwin" {
		b.WriteString("LaunchAgent installed at " + p.unitPath + "; runner at " + p.runnerPath)
	} else {
		b.WriteString("systemd user unit installed at " + p.unitPath)
	}
	b.WriteString(". Secrets in " + p.envFile + ". Logs: " + p.logs)
	b.WriteString(". Remove it with: pi-autopilot service uninstall")
	if note != "" {
		b.WriteString(note)
	}
	return b.String()
}

// ServiceInstall runs the pre-enable doctor gate (fail-closed: no write and
// no daemon call when it fails), writes the platform unit plus the owner-only
// secret file and (macOS) the sourcing runner idempotently, then starts (or
// restarts) the daemon so an upgrade or rollback re-points the unit at the
// binary that is running now. The token is stored in the secret file, never
// in the unit, so the service runs without an interactive shell exporting it.
// The message reports the host platform and the installed binary path.
func ServiceInstall(cfg Config, token string) (string, error) {
	goos := runtime.GOOS
	if goos != "darwin" && goos != "linux" {
		return "", fmt.Errorf("system service support exists for macOS and Linux, not for %s", goos)
	}
	// Pre-enable gate: runs before any filesystem write and any daemon call.
	if _, err := servicePreCheck(context.Background(), cfg); err != nil {
		return "", fmt.Errorf("pre-enable checks failed (fix the cause and re-run service install): %w", err)
	}
	plan, err := planServiceInstall(cfg, token, goos)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(plan.logs, 0700); err != nil {
		return "", err
	}
	// Write the owner-only secret file first: a failure here fails the whole
	// install before the unit references a file that does not exist.
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
			return "", fmt.Errorf("write LaunchAgent: %w", err)
		}
		if err := startLaunchAgent(); err != nil {
			return "", err
		}
	case "linux":
		if err := os.WriteFile(plan.unitPath, []byte(plan.unitBody), 0600); err != nil {
			return "", fmt.Errorf("write systemd unit: %w", err)
		}
		note, err := startSystemdUser()
		if err != nil {
			return "", err
		}
		return unitInstalledMessage(plan, note), nil
	}
	return unitInstalledMessage(plan, ""), nil
}

// ServiceInstallDryRun runs the pre-enable gate and renders every artifact
// install would write, together with the daemon actions that would run. It
// writes nothing and never prints the token value.
func ServiceInstallDryRun(cfg Config, token string) (string, error) {
	if goos := runtime.GOOS; goos != "darwin" && goos != "linux" {
		return "", fmt.Errorf("system service support exists for macOS and Linux, not for %s", goos)
	}
	if _, err := servicePreCheck(context.Background(), cfg); err != nil {
		return "", fmt.Errorf("pre-enable checks failed (fix the cause and re-run service install): %w", err)
	}
	p, err := planServiceInstall(cfg, token, runtime.GOOS)
	if err != nil {
		return "", err
	}
	return renderInstallDryRun(p), nil
}

// renderInstallDryRun is the pure dry-run report builder: it formats the plan
// (rendered artifacts, paths, modes, daemon actions) without touching the
// filesystem or the daemon, and never prints the token value. It is
// parameterized on the plan so both templates can be rendered from any host,
// which keeps the tests platform-independent.
func renderInstallDryRun(p serviceInstallPlan) string {
	var b strings.Builder
	b.WriteString("DRY RUN: no files are written and no daemon is touched.\n")
	b.WriteString("platform: " + p.platform + "\n")
	b.WriteString("binary: " + p.bin + "\n")
	fmt.Fprintf(&b, "secret file: %s (mode 0600, disposition: %s)\n", p.envFile, p.secretDisposition)
	fmt.Fprintf(&b, "unit: %s (mode 0600)\n", p.unitPath)
	if p.goos == "darwin" {
		fmt.Fprintf(&b, "runner: %s (mode 0700)\n", p.runnerPath)
		b.WriteString("--- runner ---\n" + p.runnerBody)
		b.WriteString("--- plist ---\n" + p.unitBody)
		b.WriteString("daemon actions that would run:\n  launchctl bootout gui/<uid>/" + serviceLabel + " (ignored if not loaded)\n  launchctl bootstrap gui/<uid>/" + p.unitPath + " (fallback: launchctl load -w)\n")
	} else {
		b.WriteString("--- systemd unit ---\n" + p.unitBody)
		b.WriteString("daemon actions that would run:\n  systemctl --user daemon-reload\n  systemctl --user enable " + serviceUnit + "\n  systemctl --user restart " + serviceUnit + "\n")
	}
	return b.String()
}

// ServiceUninstallDryRun reports what exists and what uninstall would remove.
// It never prints secret-file content.
func ServiceUninstallDryRun(cfg Config) (string, error) {
	if goos := runtime.GOOS; goos != "darwin" && goos != "linux" {
		return "", fmt.Errorf("system service support exists for macOS and Linux, not for %s", goos)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return renderUninstallDryRun(runtime.GOOS, home, cfg.StateDir), nil
}

// renderUninstallDryRun is the pure uninstall dry-run report builder: it
// reports what exists and what would be removed for a given goos, never
// printing secret-file content. Parameterized on goos so both templates can be
// reported from any host (platform-independent tests).
func renderUninstallDryRun(goos, home, stateDir string) string {
	var b strings.Builder
	b.WriteString("DRY RUN: no files are removed and no daemon is touched.\n")
	b.WriteString("platform: " + platformLabel() + "\n")
	exists := func(p string) string {
		if _, err := os.Stat(p); err == nil {
			return "present — would be removed"
		}
		return "absent (nothing to remove)"
	}
	if goos == "darwin" {
		unit := filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist")
		fmt.Fprintf(&b, "LaunchAgent %s: %s\n", unit, exists(unit))
		b.WriteString("daemon actions that would run:\n  launchctl bootout gui/<uid>/" + serviceLabel + "\n  launchctl unload -w <plist>\n")
	} else {
		unit := filepath.Join(home, ".config", "systemd", "user", serviceUnit)
		fmt.Fprintf(&b, "systemd user unit %s: %s\n", unit, exists(unit))
		b.WriteString("daemon actions that would run:\n  systemctl --user disable --now " + serviceUnit + "\n  systemctl --user daemon-reload\n")
	}
	if stateDir != "" {
		for _, name := range []string{serviceSecret, serviceRunner} {
			p := filepath.Join(stateDir, name)
			if _, err := os.Stat(p); err == nil {
				fmt.Fprintf(&b, "%s: present — would be removed (content is never printed)\n", p)
			} else {
				fmt.Fprintf(&b, "%s: absent\n", p)
			}
		}
	}
	return b.String()
}

// installServiceFiles writes exactly what planServiceInstall rendered. It is
// a pure filesystem seam: no daemon start, and the goos parameter means both
// templates can be exercised from any host (this is what keeps the tests
// platform-independent). Returns the unit path, a human message, and an error.
func installServiceFiles(cfg Config, token string, goos string) (string, string, error) {
	p, err := planServiceInstall(cfg, token, goos)
	if err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(p.logs, 0700); err != nil {
		return "", "", err
	}
	// Write the owner-only secret file first: a failure here fails the whole
	// install before the unit references a file that does not exist.
	if err := writeServiceSecrets(cfg.StateDir, token); err != nil {
		return "", "", err
	}
	if err := os.MkdirAll(filepath.Dir(p.unitPath), 0700); err != nil {
		return "", "", err
	}
	switch goos {
	case "darwin":
		if err := writeServiceRunner(p.runnerPath, p.bin, p.cfgPath, p.envFile); err != nil {
			return "", "", err
		}
		if err := os.WriteFile(p.unitPath, []byte(p.unitBody), 0600); err != nil {
			return "", "", fmt.Errorf("write LaunchAgent: %w", err)
		}
	case "linux":
		if err := os.WriteFile(p.unitPath, []byte(p.unitBody), 0600); err != nil {
			return "", "", fmt.Errorf("write systemd unit: %w", err)
		}
	}
	return p.unitPath, unitInstalledMessage(p, ""), nil
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
	ctx := context.Background()
	// Idempotent (re)start: bootstrap fails on an already-loaded agent and the
	// old `load -w` fallback is a silent no-op, so without booting out first an
	// upgrade would keep the stale binary running. bootout errors are ignored —
	// "not loaded" on a fresh install is exactly the expected case.
	_, _ = serviceExec(ctx, "", "launchctl", "bootout", "gui/"+uid+"/"+serviceLabel)
	// Modern bootstrap first; older launchctl only knows load.
	if _, err := serviceExec(ctx, "", "launchctl", "bootstrap", "gui/"+uid, path); err != nil {
		if _, err := serviceExec(ctx, "", "launchctl", "load", "-w", path); err != nil {
			return fmt.Errorf("LaunchAgent written to %s but launchctl could not start it: %v (try: launchctl bootstrap gui/%s %s)", path, err, uid, path)
		}
	}
	return nil
}

// startSystemdUser reloads, enables and unconditionally restarts the unit, so
// a fresh install starts it and an upgrade/rollback picks up the new
// ExecStart immediately. Returns an optional note (e.g. linger advice) to be
// appended to the install message.
func startSystemdUser() (string, error) {
	ctx := context.Background()
	if _, err := serviceExec(ctx, "", "systemctl", "--user", "daemon-reload"); err != nil {
		return "", fmt.Errorf("systemd daemon-reload: %w", err)
	}
	if out, err := serviceExec(ctx, "", "systemctl", "--user", "enable", serviceUnit); err != nil {
		return "", fmt.Errorf("systemd enable: %v (out: %s)", err, out)
	}
	// restart covers both cases: a loaded-inactive unit starts, a running
	// unit re-exec's the new binary.
	if out, err := serviceExec(ctx, "", "systemctl", "--user", "restart", serviceUnit); err != nil {
		return "", fmt.Errorf("systemd restart: %v (out: %s)", err, out)
	}
	note := ""
	if u, err := user.Current(); err == nil {
		if linger, _ := serviceExec(ctx, "", "loginctl", "show-user", u.Username, "-p", "Linger"); strings.TrimSpace(linger) != "Linger=yes" {
			note = "\nNote: the unit runs while you are logged in; run 'loginctl enable-linger' for boot start without a login session."
		}
	}
	return note, nil
}

// ServiceRestart restarts the running service with platform-native semantics:
// launchctl kickstart -k on macOS, systemctl --user restart on Linux.
func ServiceRestart() (string, error) {
	ctx := context.Background()
	switch runtime.GOOS {
	case "darwin":
		uid := strconv.Itoa(os.Getuid())
		if _, err := serviceExec(ctx, "", "launchctl", "kickstart", "-k", "gui/"+uid+"/"+serviceLabel); err != nil {
			return "", fmt.Errorf("launchctl kickstart: %w (is the LaunchAgent installed? pi-autopilot service install)", err)
		}
		return "service restarted", nil
	case "linux":
		if _, err := serviceExec(ctx, "", "systemctl", "--user", "restart", serviceUnit); err != nil {
			return "", fmt.Errorf("systemctl --user restart: %w (installed? pi-autopilot service install)", err)
		}
		return "service restarted", nil
	default:
		return "", fmt.Errorf("system service support exists for macOS and Linux, not for %s", runtime.GOOS)
	}
}

// ServiceUninstall removes the unit and (when a state_dir is known) the
// owner-only secret file and runner script, so no token lingers on disk after
// removal. cfg may be a zero value: with an empty StateDir only the fixed-path
// unit is removed, keeping uninstall robust when the config file is gone.
func ServiceUninstall(cfg Config) (string, error) {
	ctx := context.Background()
	switch runtime.GOOS {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		path := filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist")
		uid := strconv.Itoa(os.Getuid())
		_, _ = serviceExec(ctx, "", "launchctl", "bootout", "gui/"+uid+"/"+serviceLabel)
		_, _ = serviceExec(ctx, "", "launchctl", "unload", "-w", path)
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
		_, _ = serviceExec(ctx, "", "systemctl", "--user", "disable", "--now", serviceUnit)
		_, _ = serviceExec(ctx, "", "systemctl", "--user", "daemon-reload")
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
	ctx := context.Background()
	switch runtime.GOOS {
	case "darwin":
		uid := strconv.Itoa(os.Getuid())
		if _, err := serviceExec(ctx, "", "launchctl", "kickstart", "-k", "gui/"+uid+"/"+serviceLabel); err != nil {
			return "", fmt.Errorf("launchctl kickstart: %w (is the LaunchAgent installed? pi-autopilot service install)", err)
		}
		return "service started", nil
	case "linux":
		if _, err := serviceExec(ctx, "", "systemctl", "--user", "start", serviceUnit); err != nil {
			return "", fmt.Errorf("systemctl --user start: %w (installed? pi-autopilot service install)", err)
		}
		return "service started", nil
	default:
		return "", fmt.Errorf("system service support exists for macOS and Linux, not for %s", runtime.GOOS)
	}
}

func ServiceStop() (string, error) {
	ctx := context.Background()
	switch runtime.GOOS {
	case "darwin":
		if _, err := serviceExec(ctx, "", "launchctl", "stop", serviceLabel); err != nil {
			return "", fmt.Errorf("launchctl stop: %w", err)
		}
		return "service stopped", nil
	case "linux":
		if _, err := serviceExec(ctx, "", "systemctl", "--user", "stop", serviceUnit); err != nil {
			return "", fmt.Errorf("systemctl --user stop: %w (installed? pi-autopilot service install)", err)
		}
		return "service stopped", nil
	default:
		return "", fmt.Errorf("system service support exists for macOS and Linux, not for %s", runtime.GOOS)
	}
}

func ServiceStatus() (string, error) {
	ctx := context.Background()
	switch runtime.GOOS {
	case "darwin":
		out, err := serviceExec(ctx, "", "launchctl", "list")
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
		out, err := serviceExec(ctx, "", "systemctl", "--user", "show", "-p", "ActiveState,SubState,MainPID", serviceUnit)
		if err != nil {
			return "", fmt.Errorf("systemctl --user show: %w (installed? pi-autopilot service install)", err)
		}
		return "systemd user unit " + serviceUnit + ":\n" + out, nil
	default:
		return "", fmt.Errorf("system service support exists for macOS and Linux, not for %s", runtime.GOOS)
	}
}
