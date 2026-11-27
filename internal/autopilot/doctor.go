package autopilot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"
)

func Doctor(parent context.Context, cfg Config) (string, error) {
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()
	var lines []string
	lines = append(lines, "platform: "+platformLabel())
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

// DoctorBrief is the PATH-only diagnosis (issue #9, AC-1): it reports the
// platform ("<os>/<arch>") and the presence of git/gh/pi on PATH without
// probing gh auth, manager sync, Pi model inventory, or remote endpoints, and
// without a configured model. It performs no network calls and never requires
// credentials, so a `linux/arm64` build starts and self-diagnoses on a bare
// host. A missing tool is reported as a line, not an error: brief doctor is a
// launch smoke, and the full `doctor` remains the authoritative gate.
func DoctorBrief(ctx context.Context, cfg Config) string {
	var lines []string
	lines = append(lines, "platform: "+platformLabel())
	for _, tool := range []string{"git", "gh", "pi"} {
		if path, err := exec.LookPath(tool); err == nil {
			lines = append(lines, tool+": "+path)
		} else {
			lines = append(lines, tool+": not-found")
		}
	}
	return strings.Join(lines, "\n")
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
	// Web-search extension state (issue #7): SyncManagedPi above already
	// synced the managed install; report the pinned source, the verified
	// hash state and the key *source* only — never the key value (AC-3).
	ws := cfg.WebSearch
	if !ws.Enabled {
		lines = append(lines, "web search: disabled")
	} else {
		manifestPath, entryDir := webSearchPaths(cfg)
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			return "", fmt.Errorf("web search extension manifest missing (%s): re-run setup with the pinned source and sha256", manifestPath)
		}
		var m webSearchManifest
		if err := json.Unmarshal(data, &m); err != nil || m.Source != ws.Extension || m.SHA256 != ws.SHA256 {
			return "", fmt.Errorf("web search extension manifest is stale or unreadable: re-run setup with the pinned source and sha256")
		}
		h, err := hashDir(entryDir)
		if err != nil || h != ws.SHA256 {
			return "", fmt.Errorf("web search extension content no longer matches the configured sha256: re-run setup")
		}
		lines = append(lines, "web search: enabled (source: "+ws.Extension+")")
		lines = append(lines, "web search extension: verified (sha256 "+h[:12]+"...)")
		lines = append(lines, "web search key source: "+webSearchKeySource(cfg))
	}
	// Live remote endpoint/model check (issue #5): unreachable or
	// auth-failed remote endpoints are a hard doctor failure so the operator
	// gets a fix before a stage burns an attempt; a model missing from an
	// otherwise-reachable catalog is only a WARNING.
	for _, res := range CheckRemoteEndpoints(ctx, nil, cfg) {
		roles := strings.Join(res.Roles, "/")
		switch res.Status {
		case StatusOK:
			lines = append(lines, roles+" endpoint: "+res.Message)
		case StatusModelNotFound:
			lines = append(lines, "WARNING: "+roles+" endpoint: "+res.Message)
		default: // StatusUnreachable, StatusAuthFailed
			return "", fmt.Errorf("%s endpoint: %s", roles, res.Message)
		}
	}
	return strings.Join(lines, "\n"), nil
}
