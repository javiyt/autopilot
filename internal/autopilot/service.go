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
)

// A user-level service keeps the supervisor running across terminal sessions
// and restarts it on exit: a launchd LaunchAgent on macOS and a systemd user
// unit on Linux. Nothing here requires root.

const (
	serviceLabel = "com.javi.pi-autopilot"
	serviceUnit  = "pi-autopilot.service"
)

func serviceBin() (string, error) {
	bin, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(bin)
}

// ServiceInstall writes the platform unit and starts it. The Telegram token is
// stored in the unit itself so the service runs without an interactive shell
// exporting it.
func ServiceInstall(cfg Config, token string) (string, error) {
	switch runtime.GOOS {
	case "darwin":
		return installLaunchAgent(cfg, token)
	case "linux":
		return installSystemdUser(cfg, token)
	default:
		return "", fmt.Errorf("system service support exists for macOS and Linux, not for %s", runtime.GOOS)
	}
}

func installLaunchAgent(cfg Config, token string) (string, error) {
	bin, err := serviceBin()
	if err != nil {
		return "", fmt.Errorf("resolve binary: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, "Library", "LaunchAgents")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	path := filepath.Join(dir, serviceLabel+".plist")
	logs := filepath.Join(cfg.StateDir, "service-logs")
	if err := os.MkdirAll(logs, 0700); err != nil {
		return "", err
	}
	cfgPath, err := filepath.Abs(cfg.Path)
	if err != nil {
		return "", err
	}
	uid := strconv.Itoa(os.Getuid())
	plist := `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>` + serviceLabel + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + bin + `</string>
		<string>run</string>
		<string>-config</string>
		<string>` + cfgPath + `</string>
	</array>
	<key>WorkingDirectory</key>
	<string>` + filepath.Dir(cfgPath) + `</string>
	<key>EnvironmentVariables</key>
	<dict>
		<key>PATH</key>
		<string>` + os.Getenv("PATH") + `</string>
		<key>PI_AUTOPILOT_TELEGRAM_TOKEN</key>
		<string>` + token + `</string>
	</dict>
	<key>RunAtLoad</key>
	<true/>
	<key>KeepAlive</key>
	<true/>
	<key>StandardOutPath</key>
	<string>` + filepath.Join(logs, "service.out.log") + `</string>
	<key>StandardErrorPath</key>
	<string>` + filepath.Join(logs, "service.err.log") + `</string>
</dict>
</plist>
`
	if err := os.WriteFile(path, []byte(plist), 0600); err != nil {
		return "", fmt.Errorf("write LaunchAgent: %w", err)
	}
	// Modern bootstrap first; older launchctl only knows load.
	if _, err := command(context.Background(), "", "launchctl", "bootstrap", "gui/"+uid, path); err != nil {
		if _, err := command(context.Background(), "", "launchctl", "load", "-w", path); err != nil {
			return "", fmt.Errorf("LaunchAgent written to %s but launchctl could not start it: %v (try: launchctl bootstrap gui/%s %s)", path, err, uid, path)
		}
	}
	return "LaunchAgent installed at " + path + " and started. Logs: " + logs + ". Remove it with: pi-autopilot service uninstall", nil
}

func installSystemdUser(cfg Config, token string) (string, error) {
	bin, err := serviceBin()
	if err != nil {
		return "", fmt.Errorf("resolve binary: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".config", "systemd", "user")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	unitPath := filepath.Join(dir, serviceUnit)
	cfgPath, err := filepath.Abs(cfg.Path)
	if err != nil {
		return "", err
	}
	logs := filepath.Join(cfg.StateDir, "service-logs")
	if err := os.MkdirAll(logs, 0700); err != nil {
		return "", err
	}
	unit := fmt.Sprintf(`[Unit]
Description=Pi Autopilot (GitHub delivery supervisor)
After=network-online.target

[Service]
WorkingDirectory=%s
ExecStart=%s run -config %s
Environment=PATH=%s
Environment=PI_AUTOPILOT_TELEGRAM_TOKEN=%s
StandardOutput=append:%s
StandardError=append:%s
Restart=always
RestartSec=10

[Install]
WantedBy=default.target
`, filepath.Dir(cfgPath), bin, cfgPath, os.Getenv("PATH"), token, filepath.Join(logs, "service.out.log"), filepath.Join(logs, "service.err.log"))
	if err := os.WriteFile(unitPath, []byte(unit), 0600); err != nil {
		return "", fmt.Errorf("write systemd unit: %w", err)
	}
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
	return "systemd user unit installed at " + unitPath + " and started. Remove it with: pi-autopilot service uninstall." + note, nil
}

func ServiceUninstall() (string, error) {
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
		return "LaunchAgent removed from " + path, nil
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
		return "systemd user unit removed from " + unitPath, nil
	default:
		return "", fmt.Errorf("system service support exists for macOS and Linux, not for %s", runtime.GOOS)
	}
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
