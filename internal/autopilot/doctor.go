package autopilot

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strings"
	"time"
)

func Doctor(parent context.Context, cfg Config) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	var lines []string
	for _, tool := range []string{"git", "gh", "pi"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			return "", err
		}
		lines = append(lines, tool+": "+path)
	}
	out, err := command(ctx, "", "gh", "auth", "status")
	if err != nil {
		return "", fmt.Errorf("gh authentication: %w", err)
	}
	lines = append(lines, "GitHub: authenticated")
	_ = out
	managed, err := ManagedDoctor(ctx, cfg)
	if err != nil {
		return "", err
	}
	lines = append(lines, strings.Split(managed, "\n")...)
	return strings.Join(lines, "\n"), nil
}

// ManagedDoctor validates the isolated Pi installation: it syncs the managed
// agent directory from config.yaml, then runs `pi --list-models` with the
// managed environment and verifies every role model resolves. It prints key
// *sources* only — never key values (AC-3).
func ManagedDoctor(parent context.Context, cfg Config) (string, error) {
	if err := SyncManagedPi(cfg); err != nil {
		return "", fmt.Errorf("managed Pi sync: %w", err)
	}
	agentDir, sessionDir := ManagedPiDirs(cfg)
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	lines := []string{
		"managed agent dir: " + agentDir,
		"managed session dir: " + sessionDir,
	}
	models, err := commandWithEnv(ctx, "", piChildEnv(cfg), "pi", "--list-models")
	if err != nil {
		return "", fmt.Errorf("Pi model inventory (managed dir): %w", err)
	}
	for _, role := range []string{"investigate", "plan", "verify", "implement", "review", "chat", "triage"} {
		id := cfg.Models[role].ID
		if id == "" && (role == "triage" || role == "verify") {
			id = cfg.Models["plan"].ID
		}
		if id == "" {
			continue
		}
		if !strings.Contains(models, id) {
			lines = append(lines, "WARNING: model "+id+" not found in pi --list-models under the managed agent dir (verify the exact ID; older Pi builds may ignore PI_CODING_AGENT_DIR).")
		} else {
			lines = append(lines, role+" model: "+id+" (registered)")
		}
	}
	creds, err := resolvePiCredentials(cfg)
	if err != nil {
		return "", err
	}
	keys := make([]string, 0, len(creds))
	for key := range creds {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	// Key sources only: the key value itself must never be printed here.
	for _, key := range keys {
		lines = append(lines, "provider "+key+" key source: "+creds[key].Source)
	}
	return strings.Join(lines, "\n"), nil
}
