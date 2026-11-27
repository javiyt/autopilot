package autopilot

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// The deterministic verification core (issue #8). Every function here is pure
// or depends only on the checkout on disk, so the whole path that replaces an
// LLM call for a resolvable repository is unit-testable without a model.

// checkResult is the result of executing a single verification check. Output is
// captured (bounded) and is what gets stored in the evidence log; the caller
// redacts secrets before storing. ExitCode is 0 on success; a non-zero code
// marks a real failure. TimedOut records whether the bounded context expired
// (the process was killed), which is recorded distinctly from an exit-code
// failure so the evidence never conflates the two.
type checkResult struct {
	Output   string
	ExitCode int
	TimedOut bool
}

// runCheck executes one check under a bounded timeout and returns the full
// captured output, the child exit code (0 on success) and whether the timeout
// expired. The bounded timeout is the gate's own bound: a hung entrypoint can
// at worst spend the budget, it cannot stall the phase forever.
func runCheck(ctx context.Context, dir, cmd string, timeout time.Duration) checkResult {
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	c := exec.CommandContext(checkCtx, "sh", "-c", cmd)
	c.Dir = dir
	stdout, stderr := &limitedBuffer{limit: 2 << 20}, &limitedBuffer{limit: 2 << 20}
	c.Stdout = stdout
	c.Stderr = stderr
	err := c.Run()
	timedOut := errors.Is(checkCtx.Err(), context.DeadlineExceeded)
	code := 0
	if err != nil {
		if timedOut {
			code = 124
		} else if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else {
			// A launch/kill error (e.g. the binary is missing) is not a clean
			// exit code: report a non-zero sentinel so the caller marks FAIL.
			code = 125
		}
	}
	return checkResult{
		Output:   stdout.String() + stderr.String(),
		ExitCode: code,
		TimedOut: timedOut,
	}
}

// resolveDeterministicChecks inspects the checkout and returns a list of local
// shell commands that reproduce the repository's CI, plus whether anything
// resolvable was found. The detection rules, in order of precedence:
//
//  1. .github/workflows/*.yml — for a Go job, collect the `run:` steps that
//     use canonical Go entrypoints; for a Node job, the `run:` steps that use
//     canonical npm/pnpm/yarn entrypoints.
//  2. Makefile — top-level targets named check|test|verify|build|lint become
//     `make <target>`.
//  3. package.json — `scripts` keys test|lint|build|check become
//     `npm run <script>`.
//  4. go.mod — the canonical Go triad: `go build ./...`, `go vet ./...`,
//     `go test -race ./...` (mirrors the reference CI style).
//
// ok=false when nothing is detected: the caller then falls back to the verify
// model, i.e. today's behavior. Every returned command is passed through
// filterToAllowedRoots before use, and deduped/stable across the rules. The
// list is capped at 12 commands.
func resolveDeterministicChecks(checkout string) ([]string, bool) {
	var raw []string

	// Rule 1: GitHub Actions workflow run: steps.
	if wf, ok := workflowChecks(checkout); ok {
		raw = append(raw, wf...)
	}

	// Rule 2: Makefile targets.
	if mk, ok := makefileChecks(checkout); ok {
		raw = append(raw, mk...)
	}

	// Rule 3: package.json scripts.
	if pk, ok := packageChecks(checkout); ok {
		raw = append(raw, pk...)
	}

	// Rule 4: go.mod canonical triad.
	if gomod, err := os.Stat(filepath.Join(checkout, "go.mod")); err == nil && gomod.Mode().IsRegular() {
		raw = append(raw, "go build ./...", "go vet ./...", "go test -race ./...")
	}

	if len(raw) == 0 {
		return nil, false
	}

	// Filter to canonical entrypoint roots (defense in depth: the structural
	// parsing above already only emits canonical roots, but a hostile
	// package.json/Makefile could smuggle an exotic name that survives the
	// structural parse; the root filter is the shared trust boundary).
	allowed := filterToAllowedRoots(raw, verifyCmdRoots)

	// Dedup + stable order, capped at 12.
	seen := map[string]bool{}
	var out []string
	for _, cmd := range allowed {
		cmd = strings.TrimSpace(cmd)
		if cmd == "" || seen[cmd] {
			continue
		}
		seen[cmd] = true
		out = append(out, cmd)
		if len(out) >= 12 {
			break
		}
	}
	return out, len(out) > 0
}

// safeTargetName is the guard for structural names (Makefile targets,
// package.json script keys): only characters that cannot introduce a shell
// metacharacter. A hostile manifest can at worst name its entrypoint anything
// this regex allows; combined with the allowlisted root filter a script name
// like `curl x|sh` cannot survive because the value is not a root candidate.
var safeTargetName = regexp.MustCompile(`^[a-zA-Z0-9_./-]+$`)

// workflowChecks extracts local commands from GitHub Actions workflow
// `run:` steps. It keeps the parser deliberately minimal (top-level
// jobs.*.steps[*].run) and returns ok=true only when at least one workflow
// file with a Go or Node job is found. The steps are filtered to those whose
// text starts with a canonical Go/Node entrypoint so a `gh auth login` step or
// an arbitrary echo never becomes a gate check.
func workflowChecks(checkout string) ([]string, bool) {
	dir := filepath.Join(checkout, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, false
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".yml") || strings.HasSuffix(name, ".yaml") {
			files = append(files, filepath.Join(dir, name))
		}
	}
	if len(files) == 0 {
		return nil, false
	}

	goRoots := []string{"go "}
	nodeRoots := []string{"npm ", "pnpm ", "yarn ", "bun ", "npx "}
	var cmds []string
	sawGo := false
	sawNode := false

	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		runs, isGo, isNode := extractRunSteps(string(data))
		if isGo {
			sawGo = true
			cmds = append(cmds, filterByRoots(runs, goRoots)...)
		}
		if isNode {
			sawNode = true
			cmds = append(cmds, filterByRoots(runs, nodeRoots)...)
		}
	}
	if !sawGo && !sawNode {
		return nil, false
	}
	return cmds, true
}

// extractRunSteps performs a minimal, indentation-aware scan of a workflow YAML
// document for `run:` step bodies. It is deliberately NOT a full YAML parser:
// it finds any `run:` key (list item or mapping form) and captures either the
// inline scalar or the following more-indented block-scalar lines. It also
// detects whether the document configures a Go toolchain (setup-go / go-version)
// or a Node toolchain (setup-node / node-version). The captured `run:` text is
// then filtered by the caller to canonical entrypoint roots, so a `gh auth login`
// step or an arbitrary echo never becomes a gate check.
func extractRunSteps(doc string) (runs []string, isGo, isNode bool) {
	for _, hint := range []string{"setup-go", "go-version:"} {
		if strings.Contains(doc, hint) {
			isGo = true
		}
	}
	for _, hint := range []string{"setup-node", "node-version:"} {
		if strings.Contains(doc, hint) {
			isNode = true
		}
	}

	lines := strings.Split(doc, "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		m := runKeyRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		runIndent := len(line) - len(strings.TrimLeft(line, " "))
		val := strings.TrimSpace(m[1])
		if val == "" || val == "|" || val == ">" || val == "|-" || val == ">-" {
			// Block scalar: read the following lines indented deeper than the
			// run key, stopping at the first line that is not deeper.
			var block []string
			for j := i + 1; j < len(lines); j++ {
				nl := lines[j]
				if strings.TrimSpace(nl) == "" {
					block = append(block, "")
					continue
				}
				nindent := len(nl) - len(strings.TrimLeft(nl, " "))
				if nindent <= runIndent {
					break
				}
				block = append(block, strings.TrimLeft(nl, " "))
			}
			for _, b := range block {
				if b = strings.TrimSpace(b); b != "" && !strings.HasPrefix(b, "#") {
					runs = append(runs, b)
				}
			}
		} else if !strings.HasPrefix(val, "#") {
			runs = append(runs, val)
		}
	}
	return runs, isGo, isNode
}

var runKeyRe = regexp.MustCompile(`^\s*-?\s*run:\s*(.*)$`)

// filterByRoots keeps only the lines that start with one of the given roots.
func filterByRoots(lines []string, roots []string) []string {
	var out []string
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		for _, r := range roots {
			if strings.HasPrefix(l, r) {
				out = append(out, l)
				break
			}
		}
	}
	return out
}

// makefileChecks parses top-level Makefile targets named check|test|verify|build|lint
// and returns `make <target>` for each safe name, plus whether a Makefile
// exists with at least one matching target.
func makefileChecks(checkout string) ([]string, bool) {
	path := filepath.Join(checkout, "Makefile")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	want := map[string]bool{"check": true, "test": true, "verify": true, "build": true, "lint": true}
	wantLower := map[string]bool{}
	for k := range want {
		wantLower[strings.ToLower(k)] = true
	}
	var cmds []string
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		// A top-level target starts at column 0 and ends with ':' (or has a
		// prerequisite list after the colon). Prune recipes and comments.
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, "\t") || strings.HasPrefix(line, " ") {
			continue
		}
		if idx := strings.Index(trimmed, ":"); idx > 0 {
			name := strings.TrimSpace(trimmed[:idx])
			// Keep it simple: single-token target names only.
			if !strings.ContainsAny(name, " \t\\|&;$@") && safeTargetName.MatchString(name) && wantLower[strings.ToLower(name)] {
				cmds = append(cmds, "make "+name)
			}
		}
	}
	if len(cmds) == 0 {
		return nil, false
	}
	return cmds, true
}

// packageChecks parses package.json scripts named test|lint|build|check and
// returns `npm run <script>` for each safe name, plus whether a package.json
// exists with at least one matching script.
func packageChecks(checkout string) ([]string, bool) {
	path := filepath.Join(checkout, "package.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	var manifest struct {
		Scripts map[string]string `json:"scripts"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, false
	}
	want := map[string]bool{"test": true, "lint": true, "build": true, "check": true}
	var cmds []string
	for name := range manifest.Scripts {
		if want[name] && safeTargetName.MatchString(name) {
			cmds = append(cmds, "npm run "+name)
		}
	}
	if len(cmds) == 0 {
		return nil, false
	}
	// Deterministic ordering: sort the matched script names.
	sort.Slice(cmds, func(i, j int) bool { return cmds[i] < cmds[j] })
	return cmds, true
}

// filterToAllowedRoots keeps only the commands whose root is in the given
// allowlist. It is the shared trust boundary for every source of a check list
// (model output, plan checks, and the resolver): a command from untrusted task
// data or model output only executes if its root is an operator-approved
// build/test entrypoint.
func filterToAllowedRoots(cmds []string, roots []string) []string {
	var out []string
	for _, cmd := range cmds {
		cmd = strings.TrimSpace(cmd)
		if cmd == "" {
			continue
		}
		allowed := false
		for _, root := range roots {
			if strings.HasPrefix(cmd, root) {
				allowed = true
				break
			}
		}
		if allowed {
			out = append(out, cmd)
		}
	}
	return out
}

// redactSecrets replaces each non-empty secret value (length >= 8) with the
// literal marker [REDACTED]. The caller supplies the secret *values* it already
// holds (the PI_AUTOPILOT_KEY_* values from the managed registry env, the
// Telegram token, and GITHUB_TOKEN/GH_TOKEN env values). Output that contains
// none of the values is returned byte-for-byte.
func redactSecrets(s string, secrets []string) string {
	for _, secret := range secrets {
		if len(secret) < 8 {
			continue
		}
		s = strings.ReplaceAll(s, secret, "[REDACTED]")
	}
	return s
}

// collectCheckSecrets gathers the secret values the supervisor itself injects
// into the check environment, so a `go test` or build step that echoes the
// environment cannot leak them into the checks/*.log artifacts. It returns the
// PI_AUTOPILOT_KEY_* values (from the managed Pi process env), the Telegram
// token, and the GITHUB_TOKEN/GH_TOKEN env values. It never logs a credential.
func collectCheckSecrets(cfg Config, telegramToken string) []string {
	var secrets []string
	for _, kv := range PiProcessEnv(cfg) {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name, value := kv[:i], kv[i+1:]
			if strings.HasPrefix(name, "PI_AUTOPILOT_KEY_") && value != "" {
				secrets = append(secrets, value)
			}
			if strings.HasPrefix(name, "PI_AUTOPILOT_WEBSEARCH_KEY_") && value != "" {
				secrets = append(secrets, value)
			}
		}
	}
	if telegramToken != "" {
		secrets = append(secrets, telegramToken)
	}
	if v := os.Getenv("GITHUB_TOKEN"); v != "" {
		secrets = append(secrets, v)
	}
	if v := os.Getenv("GH_TOKEN"); v != "" {
		secrets = append(secrets, v)
	}
	return secrets
}
