package autopilot

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Web-search extension tests (issue #7) follow the repository fake-`pi`
// convention: a shell stub prepended to PATH records `install` invocations,
// mimes a pinned npm install under $PI_CODING_AGENT_DIR, dumps argv/env for
// session runs and answers --list-models for the doctor.

const (
	fakeWebSearchPackageJSON = `{"name":"pi-websearch-fake","pi":{"extensions":["./index.js"]}}`
	fakeWebSearchIndexJS     = `// fake web-search extension`
)

// fakeWebSearchHash computes the deterministic sha256 of the fake installed
// package: the reference bytes the fake `pi install` writes and the bytes a
// tamper test mutates must hash through the same hashDir code path.
func fakeWebSearchHash(t *testing.T) string {
	t.Helper()
	ref := t.TempDir()
	if err := os.WriteFile(filepath.Join(ref, "package.json"), []byte(fakeWebSearchPackageJSON+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(ref, "index.js"), []byte(fakeWebSearchIndexJS+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	h, err := hashDir(ref)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func fakeWebSearchPi(t *testing.T) (binDir, dumpDir string) {
	t.Helper()
	binDir = t.TempDir()
	dumpDir = t.TempDir()
	script := `#!/bin/sh
if [ "$1" = "install" ]; then
  echo "$1 $2" >> "$AUTOPILOT_TEST_DUMP/installs.txt"
  spec=${2#npm:}
  name=${spec%@*}
  root="$PI_CODING_AGENT_DIR/npm/node_modules/$name"
  mkdir -p "$root"
  printf '%s\n' '{"name":"pi-websearch-fake","pi":{"extensions":["./index.js"]}}' > "$root/package.json"
  printf '%s\n' '// fake web-search extension' > "$root/index.js"
  exit 0
fi
if [ "$1" = "--list-models" ]; then
  printf '%s\n' 'omlx: reviewer' 'omlx: planner' 'omlx: coder'
  exit 0
fi
printf '%s\n' "$@" > "$AUTOPILOT_TEST_DUMP/argv.txt"
env | sort > "$AUTOPILOT_TEST_DUMP/env.txt"
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"ok"}]}}' '{"type":"agent_settled","aborted":false}'
`
	if err := os.WriteFile(filepath.Join(binDir, "pi"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AUTOPILOT_TEST_DUMP", dumpDir)
	return binDir, dumpDir
}

// webSearchEnabledCfg returns testConfig plus an enabled web_search block
// pinned to the fake package (hash computed from the reference bytes).
func webSearchEnabledCfg(stateDir, sha string) Config {
	cfg := testConfig(stateDir)
	cfg.WebSearch = WebSearch{
		Enabled:        true,
		Extension:      "npm:pi-websearch-fake@1.0.0",
		SHA256:         sha,
		AllowedRoles:   []string{"investigate", "plan"},
		Backend:        "searxng",
		BackendURL:     "http://searxng.example:8080",
		TimeoutSeconds: 60,
	}
	return cfg
}

func argvFields(t *testing.T, dumpDir string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dumpDir, "argv.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(data))
}

func hasField(fields []string, want string) bool {
	for _, f := range fields {
		if f == want {
			return true
		}
	}
	return false
}

func fieldAfter(fields []string, flag string) (string, bool) {
	for i, f := range fields {
		if f == flag && i+1 < len(fields) {
			return fields[i+1], true
		}
	}
	return "", false
}

// AC-1: an allowlisted role receives the pinned extension via explicit -e,
// its tool in --tools, --no-extensions kept, the untrusted-data directive,
// and the PI_AUTOPILOT_WEBSEARCH_* child env.
func TestWebSearchPhaseActivation(t *testing.T) {
	_, dumpDir := fakeWebSearchPi(t)
	stateDir := t.TempDir()
	cfg := webSearchEnabledCfg(stateDir, fakeWebSearchHash(t))
	if err := SyncWebSearchExtension(context.Background(), cfg); err != nil {
		t.Fatalf("sync must install the pinned fake extension: %v", err)
	}
	res := RunPi(context.Background(), cfg, stateDir, "investigate", "omlx", "reviewer", "low", "test", filepath.Join(stateDir, "out.jsonl"), true)
	if res.Err != nil {
		t.Fatalf("allowlisted session must succeed: %v", res.Err)
	}
	fields := argvFields(t, dumpDir)
	if !hasField(fields, "--no-extensions") {
		t.Fatalf("discovery must stay disabled: %v", fields)
	}
	entry := filepath.Join(stateDir, "pi", "agent", "extensions", "pi-websearch")
	if !hasField(fields, "-e") || !hasField(fields, entry) {
		t.Fatalf("allowlisted role must load the pinned extension via explicit -e: %v", fields)
	}
	if tools, ok := fieldAfter(fields, "--tools"); !ok || tools != "read,grep,find,ls,web_search" {
		t.Fatalf("web_search tool must be the only addition to the read-only allowlist: %v", fields)
	}
	if !hasField(fields, "--append-system-prompt") {
		t.Fatalf("untrusted-data directive must be appended: %v", fields)
	}
	envData, err := os.ReadFile(filepath.Join(dumpDir, "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	envText := string(envData)
	for _, want := range []string{
		"PI_AUTOPILOT_WEBSEARCH_BACKEND=searxng",
		"PI_AUTOPILOT_WEBSEARCH_URL=http://searxng.example:8080",
		"PI_AUTOPILOT_WEBSEARCH_TIMEOUT_SECONDS=60",
		"PI_CODING_AGENT_DIR=" + filepath.Join(stateDir, "pi", "agent"),
	} {
		if !strings.Contains(envText, want) {
			t.Fatalf("child env missing %s: %s", want, envText)
		}
	}
}

// AC-2: with web_search absent or disabled every argv is exactly today's
// baseline (no -e anywhere; --no-approve present as the new defense-in-depth
// floor for ALL sessions).
func TestWebSearchDisabledByDefault(t *testing.T) {
	_, dumpDir := fakeWebSearchPi(t)
	stateDir := t.TempDir()
	const wantArgv = "--mode json --no-session --no-extensions --no-mcp --no-skills --no-prompt-templates --no-approve --provider omlx --model reviewer --thinking low --tools read,grep,find,ls -- test"
	cases := map[string]Config{
		"absent": testConfig(stateDir),
		"disabled": func() Config {
			cfg := testConfig(stateDir)
			cfg.WebSearch = WebSearch{Enabled: false}
			return cfg
		}(),
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			res := RunPi(context.Background(), cfg, stateDir, "investigate", "omlx", "reviewer", "low", "test", filepath.Join(stateDir, "out.jsonl"), true)
			if res.Err != nil {
				t.Fatalf("session must succeed: %v", res.Err)
			}
			data, err := os.ReadFile(filepath.Join(dumpDir, "argv.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(strings.Fields(string(data)), " "); got != wantArgv {
				t.Fatalf("argv must be exactly the baseline (no -e, same --tools):\n got %q\nwant %q", got, wantArgv)
			}
		})
	}
}

// AC-2/AC-3: implement and review argv never carry the extension or its
// tool, and --no-approve is present in every session; an allowlisted role
// keeps the extension as the contrast.
func TestWebSearchToolAllowlist(t *testing.T) {
	_, dumpDir := fakeWebSearchPi(t)
	stateDir := t.TempDir()
	cfg := webSearchEnabledCfg(stateDir, fakeWebSearchHash(t))
	if err := SyncWebSearchExtension(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		role     string
		readOnly bool
		tools    string
	}{
		{"implement", false, "read,grep,find,ls,bash,edit,write"},
		{"review", true, "read,grep,find,ls"},
	} {
		t.Run(tc.role, func(t *testing.T) {
			res := RunPi(context.Background(), cfg, stateDir, tc.role, "omlx", "reviewer", "low", "test", filepath.Join(stateDir, "out.jsonl"), tc.readOnly)
			if res.Err != nil {
				t.Fatalf("session must succeed: %v", res.Err)
			}
			fields := argvFields(t, dumpDir)
			if hasField(fields, "-e") {
				t.Fatalf("%s must never load the extension: %v", tc.role, fields)
			}
			if tools, _ := fieldAfter(fields, "--tools"); tools != tc.tools {
				t.Fatalf("%s must keep today's tool allowlist: got %q want %q", tc.role, tools, tc.tools)
			}
			if !hasField(fields, "--no-approve") {
				t.Fatalf("every session must pass --no-approve: %v", fields)
			}
		})
	}
	res := RunPi(context.Background(), cfg, stateDir, "investigate", "omlx", "reviewer", "low", "test", filepath.Join(stateDir, "out.jsonl"), true)
	if res.Err != nil {
		t.Fatalf("allowlisted session must succeed: %v", res.Err)
	}
	fields := argvFields(t, dumpDir)
	if !hasField(fields, "-e") {
		t.Fatalf("allowlisted role keeps the contrast: -e present: %v", fields)
	}
	if !hasField(fields, "--no-approve") {
		t.Fatalf("allowlisted session must pass --no-approve: %v", fields)
	}
}

// AC-3: allowed_roles policy — implement/review are rejected at ReadConfig
// time, and an enabled block needs a non-empty allowlist.
func TestWebSearchRolePolicy(t *testing.T) {
	dir := t.TempDir()
	const sha = "a123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	base := `repo: owner/repo
state_dir: ./work
check_commands: ["go test ./..."]
models:
  investigate: {provider: ollama, id: reviewer, thinking: low}
  plan: {provider: ollama, id: planner, thinking: medium}
  implement: {provider: ollama, id: coder, thinking: "off"}
  review: {provider: ollama, id: reviewer, thinking: low}
  chat: {provider: ollama, id: reviewer, thinking: low}
allowed_models: [planner, coder, reviewer]
telegram_chat_id: 123
`
	block := `web_search:
  enabled: true
  extension: "npm:pi-websearch-fake@1.0.0"
  sha256: "` + sha + `"
`
	cases := map[string]string{
		"implement": block + "  allowed_roles: [implement]\n  backend: searxng\n  backend_url: http://searxng.example:8080\n",
		"review":    block + "  allowed_roles: [review]\n  backend: searxng\n  backend_url: http://searxng.example:8080\n",
		"empty":     block + "  backend: searxng\n  backend_url: http://searxng.example:8080\n",
	}
	for name, ws := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, "config.yaml")
			if err := os.WriteFile(path, []byte(base+ws), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := ReadConfig(path); err == nil {
				t.Fatalf("web_search.allowed_roles with %s must be rejected at config load", name)
			}
		})
	}
	// A valid block must load.
	path := filepath.Join(dir, "config.yaml")
	valid := block + "  allowed_roles: [investigate, plan]\n  backend: searxng\n  backend_url: http://searxng.example:8080\n  timeout_seconds: 60\n"
	if err := os.WriteFile(path, []byte(base+valid), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ReadConfig(path)
	if err != nil {
		t.Fatalf("valid web_search block must load: %v", err)
	}
	if !cfg.WebSearchRoleAllowed("investigate") || cfg.WebSearchRoleAllowed("implement") {
		t.Fatalf("role allowlist wrong: %+v", cfg.WebSearch.AllowedRoles)
	}
}

// AC-3: tamper → sync fails closed and purges; an allowlisted role then
// fails the stage instead of silently running extension-free.
func TestWebSearchIntegrity(t *testing.T) {
	fakeWebSearchPi(t)
	stateDir := t.TempDir()
	cfg := webSearchEnabledCfg(stateDir, fakeWebSearchHash(t))
	if err := SyncWebSearchExtension(context.Background(), cfg); err != nil {
		t.Fatalf("initial sync must succeed: %v", err)
	}
	entry := filepath.Join(stateDir, "pi", "agent", "extensions", "pi-websearch")
	manifest := filepath.Join(stateDir, "pi", "agent", "websearch.json")
	if _, err := os.Stat(entry); err != nil {
		t.Fatalf("entry dir must exist after sync: %v", err)
	}
	// Tamper with the installed content.
	if err := os.WriteFile(filepath.Join(entry, "index.js"), []byte("malicious\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := SyncWebSearchExtension(context.Background(), cfg); err == nil {
		t.Fatal("tampered content must fail the sync (fail closed)")
	}
	if _, err := os.Stat(entry); !os.IsNotExist(err) {
		t.Fatal("tampered entry must be purged")
	}
	if _, err := os.Stat(manifest); !os.IsNotExist(err) {
		t.Fatal("manifest must be purged with the entry")
	}
	// No silent fallback: an allowlisted role fails closed.
	res := RunPi(context.Background(), cfg, stateDir, "investigate", "omlx", "reviewer", "low", "test", filepath.Join(stateDir, "out.jsonl"), true)
	if res.Err == nil {
		t.Fatal("allowlisted role must fail when the verified extension is gone")
	}
	if !strings.Contains(res.Err.Error(), "web search extension") {
		t.Fatalf("failure must name the web search extension: %v", res.Err)
	}
}

// AC-3: a literal api_key travels only in the child environment as
// PI_AUTOPILOT_WEBSEARCH_KEY_<BACKEND> — never in argv, never on disk; ${VAR}
// references pass through inherited env only; doctor prints sources only.
func TestWebSearchCredentialSafety(t *testing.T) {
	const secret = "SENTINEL-SECRET-DO-NOT-LEAK"
	_, dumpDir := fakeWebSearchPi(t)
	stateDir := t.TempDir()
	cfg := webSearchEnabledCfg(stateDir, fakeWebSearchHash(t))
	cfg.WebSearch.APIKey = secret // literal (rawAPIKey empty ⇒ treated as literal)
	if err := SyncWebSearchExtension(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	res := RunPi(context.Background(), cfg, stateDir, "investigate", "omlx", "reviewer", "low", "test", filepath.Join(stateDir, "out.jsonl"), true)
	if res.Err != nil {
		t.Fatalf("session must succeed: %v", res.Err)
	}
	data, err := os.ReadFile(filepath.Join(dumpDir, "argv.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatalf("secret must never appear in argv: %s", data)
	}
	envData, err := os.ReadFile(filepath.Join(dumpDir, "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	envText := string(envData)
	if strings.Count(envText, secret) != 1 {
		t.Fatalf("secret must appear exactly once in child env: %s", envText)
	}
	if !strings.Contains(envText, "PI_AUTOPILOT_WEBSEARCH_KEY_SEARXNG="+secret) {
		t.Fatal("literal key must reach the child env as PI_AUTOPILOT_WEBSEARCH_KEY_SEARXNG")
	}
	// Nothing on disk in the managed agent dir may carry the key value.
	agentDir := filepath.Join(stateDir, "pi", "agent")
	err = filepath.WalkDir(agentDir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.Contains(string(content), secret) {
			t.Fatalf("secret leaked to disk: %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// Doctor prints key sources only — never the value (checked while the
	// literal key is still active, before switching to the env reference).
	out, err := ManagedDoctor(context.Background(), cfg)
	if err != nil {
		t.Fatalf("doctor must pass with a verified install: %v", err)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("doctor output leaked the key value: %s", out)
	}
	if !strings.Contains(out, "literal in YAML (child env: PI_AUTOPILOT_WEBSEARCH_KEY_SEARXNG)") {
		t.Fatalf("doctor must print the key source: %s", out)
	}
	if !strings.Contains(out, "web search extension: verified") {
		t.Fatalf("doctor must report the verified hash state: %s", out)
	}
	// Env reference: the referenced variable passes through the inherited
	// environment; nothing is injected under PI_AUTOPILOT_WEBSEARCH_KEY_*.
	t.Setenv("SEARXNG_API_KEY", secret)
	cfg.WebSearch.rawAPIKey = "${SEARXNG_API_KEY}"
	cfg.WebSearch.APIKey = secret
	res = RunPi(context.Background(), cfg, stateDir, "investigate", "omlx", "reviewer", "low", "test", filepath.Join(stateDir, "out.jsonl"), true)
	if res.Err != nil {
		t.Fatalf("session must succeed: %v", res.Err)
	}
	envData, err = os.ReadFile(filepath.Join(dumpDir, "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	envText = string(envData)
	if strings.Count(envText, secret) != 1 || !strings.Contains(envText, "SEARXNG_API_KEY="+secret) {
		t.Fatalf("env reference must pass through inherited env exactly once: %s", envText)
	}
	if strings.Contains(envText, "PI_AUTOPILOT_WEBSEARCH_KEY_") {
		t.Fatalf("env references must not be injected: %s", envText)
	}
}

// AC-4: a second sync with unchanged hash performs no write and no
// `pi install` re-run.
func TestWebSearchInstallerIdempotent(t *testing.T) {
	_, dumpDir := fakeWebSearchPi(t)
	stateDir := t.TempDir()
	cfg := webSearchEnabledCfg(stateDir, fakeWebSearchHash(t))
	if err := SyncWebSearchExtension(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	installs, err := os.ReadFile(filepath.Join(dumpDir, "installs.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(installs)); got != "install npm:pi-websearch-fake@1.0.0" {
		t.Fatalf("first sync must install exactly once: %q", got)
	}
	manifestPath := filepath.Join(stateDir, "pi", "agent", "websearch.json")
	first, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := SyncWebSearchExtension(context.Background(), cfg); err != nil {
		t.Fatalf("second sync must be a no-op: %v", err)
	}
	installs, err = os.ReadFile(filepath.Join(dumpDir, "installs.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(installs)); got != "install npm:pi-websearch-fake@1.0.0" {
		t.Fatalf("second sync must not re-run pi install: %q", got)
	}
	second, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("second sync must not rewrite the manifest")
	}
}

// AC-4: pin-format and bound validation at ReadConfig; exact version and
// commit hash accepted, mutable refs rejected; known-field typos rejected.
func TestReadConfigWebSearch(t *testing.T) {
	dir := t.TempDir()
	const sha = "a123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	base := `repo: owner/repo
state_dir: ./work
check_commands: ["go test ./..."]
models:
  investigate: {provider: ollama, id: reviewer, thinking: low}
  plan: {provider: ollama, id: planner, thinking: medium}
  implement: {provider: ollama, id: coder, thinking: "off"}
  review: {provider: ollama, id: reviewer, thinking: low}
  chat: {provider: ollama, id: reviewer, thinking: low}
allowed_models: [planner, coder, reviewer]
telegram_chat_id: 123
`
	validBlock := func() string {
		return `web_search:
  enabled: true
  extension: "npm:pi-websearch-fake@1.0.0"
  sha256: "` + sha + `"
  allowed_roles: [investigate]
  backend: searxng
  backend_url: http://searxng.example:8080
  timeout_seconds: 60
`
	}
	read := func(t *testing.T, body string) (Config, error) {
		t.Helper()
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		return ReadConfig(path)
	}
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"valid-npm-exact", base + validBlock(), false},
		{"valid-git-commit", base + `web_search:
  enabled: true
  extension: "git:github.com/example/pi-websearch@` + strings.Repeat("9", 40) + `"
  sha256: "` + sha + `"
  allowed_roles: [plan]
  backend: searxng
  backend_url: https://searxng.example
  timeout_seconds: 60
`, false},
		{"reject-latest", base + strings.Replace(validBlock(), "pi-websearch-fake@1.0.0", "pi-websearch-fake@latest", 1), true},
		{"reject-caret-range", base + strings.Replace(validBlock(), "pi-websearch-fake@1.0.0", "pi-websearch-fake@^1.0.0", 1), true},
		{"reject-x-range", base + strings.Replace(validBlock(), "pi-websearch-fake@1.0.0", "pi-websearch-fake@1.x", 1), true},
		{"reject-bare-npm", base + strings.Replace(validBlock(), "pi-websearch-fake@1.0.0", "pi-websearch-fake", 1), true},
		{"reject-git-branch", base + strings.Replace(validBlock(), "npm:pi-websearch-fake@1.0.0", "git:github.com/example/pi-websearch@main", 1), true},
		{"reject-short-hash", base + strings.Replace(validBlock(), "npm:pi-websearch-fake@1.0.0", "git:github.com/example/pi-websearch@deadbeef", 1), true},
		{"reject-bad-sha", base + strings.Replace(validBlock(), sha, "zz", 1), true},
		{"reject-ftp-url", base + strings.Replace(validBlock(), "http://searxng.example:8080", "ftp://searxng.example:8080", 1), true},
		{"reject-timeout-low", base + strings.Replace(validBlock(), "timeout_seconds: 60", "timeout_seconds: 4", 1), true},
		{"reject-timeout-high", base + strings.Replace(validBlock(), "timeout_seconds: 60", "timeout_seconds: 601", 1), true},
		{"reject-typo-field", base + strings.Replace(validBlock(), "backend: searxng", "backendd: searxng", 1), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := read(t, tc.body)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("config must be rejected: %s", tc.body)
				}
				return
			}
			if err != nil {
				t.Fatalf("config must load: %v", err)
			}
			_ = cfg
		})
	}
	// timeout_seconds 0 defaults to 120 for enabled blocks.
	body := strings.Replace(validBlock(), "  timeout_seconds: 60\n", "", 1)
	cfg, err := read(t, base+body)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WebSearch.TimeoutSeconds != 120 {
		t.Fatalf("default timeout must apply: %d", cfg.WebSearch.TimeoutSeconds)
	}
}

// Rollback: disabling purges a previously installed extension.
func TestWebSearchDisabledPurgesStaleInstall(t *testing.T) {
	fakeWebSearchPi(t)
	stateDir := t.TempDir()
	cfg := webSearchEnabledCfg(stateDir, fakeWebSearchHash(t))
	if err := SyncWebSearchExtension(context.Background(), cfg); err != nil {
		t.Fatal(err)
	}
	entry := filepath.Join(stateDir, "pi", "agent", "extensions", "pi-websearch")
	manifest := filepath.Join(stateDir, "pi", "agent", "websearch.json")
	if _, err := os.Stat(entry); err != nil {
		t.Fatal(err)
	}
	cfg.WebSearch.Enabled = false
	if err := SyncWebSearchExtension(context.Background(), cfg); err != nil {
		t.Fatalf("disabled sync must be a no-op purge: %v", err)
	}
	if _, err := os.Stat(entry); !os.IsNotExist(err) {
		t.Fatal("stale entry dir must be purged")
	}
	if _, err := os.Stat(manifest); !os.IsNotExist(err) {
		t.Fatal("stale manifest must be purged")
	}
}
