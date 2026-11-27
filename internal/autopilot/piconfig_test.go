package autopilot

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestManagedPiDirsDefaultAndOverride(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	cfg := testConfig(stateDir)
	agentDir, sessionDir := ManagedPiDirs(cfg)
	if agentDir != filepath.Join(stateDir, "pi", "agent") {
		t.Fatalf("default agent dir = %q, want %q", agentDir, filepath.Join(stateDir, "pi", "agent"))
	}
	if sessionDir != filepath.Join(stateDir, "pi", "sessions") {
		t.Fatalf("session dir = %q, want under state_dir/pi/sessions", sessionDir)
	}

	cfg.PiAgentDir = "custom/pi"
	agentDir, _ = ManagedPiDirs(cfg)
	if agentDir != filepath.Join(stateDir, "custom", "pi") {
		t.Fatalf("relative override = %q, want under state_dir", agentDir)
	}
	cfg.PiAgentDir = "/abs/agent"
	agentDir, _ = ManagedPiDirs(cfg)
	if agentDir != "/abs/agent" {
		t.Fatalf("absolute override = %q, want /abs/agent", agentDir)
	}
}

func TestPiProviderKey(t *testing.T) {
	cases := []struct {
		profile ModelProfile
		want    string
	}{
		{ModelProfile{Provider: "omlx"}, "omlx"},
		{ModelProfile{Provider: "omlx", BaseURL: "https://mac-studio:8000/v1"}, "omlx-remote-mac-studio:8000"},
		{ModelProfile{Provider: "omlx", BaseURL: "http://localhost:8080"}, "omlx-remote-localhost:8080"},
		{ModelProfile{Provider: "ollama"}, "ollama"},
		{ModelProfile{Provider: "ollama", BaseURL: "http://other-host:11434"}, "ollama"},
		{ModelProfile{Provider: "openai"}, "openai"},
		{ModelProfile{Provider: "openai", BaseURL: "https://api.openai.com/v1"}, "openai"},
		{ModelProfile{Provider: "openai", BaseURL: "https://proxy.example.com/v1"}, "openai-proxy.example.com"},
		{ModelProfile{Provider: "openai-compatible", BaseURL: "http://llm.example.com/v1"}, "openai-compatible-llm.example.com"},
		{ModelProfile{Provider: "anthropic"}, "anthropic"},
		{ModelProfile{Provider: "anthropic", BaseURL: "https://api.anthropic.com/v1"}, "anthropic"},
		{ModelProfile{Provider: "anthropic-compatible", BaseURL: "https://gw.example.com/v1"}, "anthropic-compatible-gw.example.com"},
		{ModelProfile{Provider: "myservice"}, "myservice"},
	}
	for _, c := range cases {
		if got := PiProviderKey(c.profile); got != c.want {
			t.Errorf("PiProviderKey(%+v) = %q, want %q", c.profile, got, c.want)
		}
	}
}

func TestPiProcessEnvSetsManagedDirs(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	cfg := testConfig(stateDir)
	cfg.Models["plan"] = ModelProfile{Provider: "openai-compatible", BaseURL: "http://llm.example.com/v1", ID: "x", APIKey: "literal-secret"}
	env := PiProcessEnv(cfg)
	seen := map[string]string{}
	for _, kv := range env {
		k, v, ok := strings.Cut(kv, "=")
		if ok {
			seen[k] = v
		}
	}
	wantAgent := filepath.Join(stateDir, "pi", "agent")
	if seen["PI_CODING_AGENT_DIR"] != wantAgent {
		t.Fatalf("PI_CODING_AGENT_DIR = %q, want %q", seen["PI_CODING_AGENT_DIR"], wantAgent)
	}
	if seen["PI_CODING_AGENT_SESSION_DIR"] != filepath.Join(stateDir, "pi", "sessions") {
		t.Fatalf("PI_CODING_AGENT_SESSION_DIR = %q", seen["PI_CODING_AGENT_SESSION_DIR"])
	}
	if got := seen["PI_AUTOPILOT_KEY_OPENAI_COMPATIBLE_LLM_EXAMPLE_COM"]; got != "literal-secret" {
		t.Fatalf("literal key must ride in child env as PI_AUTOPILOT_KEY_*, got %q in %+v", got, seen)
	}
	for _, kv := range env {
		if strings.Contains(kv, "literal-secret") && !strings.HasPrefix(kv, "PI_AUTOPILOT_KEY_") {
			t.Fatalf("secret leaked outside PI_AUTOPILOT_KEY_* env entries: %q", kv)
		}
	}
}

func modelsFromBuild(t *testing.T, cfg Config) map[string]piProvider {
	t.Helper()
	data, err := BuildManagedModelsJSON(cfg)
	if err != nil {
		t.Fatalf("BuildManagedModelsJSON: %v", err)
	}
	var doc piModelsFile
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("generated models.json is not valid JSON: %v", err)
	}
	if doc.Schema != piModelsSchema {
		t.Fatalf("missing $schema: %q", doc.Schema)
	}
	return doc.Providers
}

func TestBuildManagedModelsJSONProviders(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	base := func() Config {
		cfg := testConfig(stateDir)
		cfg.AllowedModels = []string{}
		return cfg
	}

	// ollama local: /v1 suffix appended, dummy key
	cfg := base()
	for role := range cfg.Models {
		cfg.Models[role] = ModelProfile{Provider: "ollama", ID: "qwen", Thinking: "off"}
	}
	provs := modelsFromBuild(t, cfg)
	p := provs["ollama"]
	if p.BaseURL != "http://localhost:11434/v1" || p.API != "openai-completions" || p.APIKey != "ollama" {
		t.Fatalf("ollama local unexpected: %+v", p)
	}
	if p.BaseURL == "" {
		t.Fatal("ollama baseUrl must be non-empty")
	}

	// omlx local: built-in provider, models only
	cfg = base()
	provs = modelsFromBuild(t, cfg) // testConfig uses omlx with no base_url
	p = provs["omlx"]
	if p.BaseURL != "" || p.API != "" || p.APIKey != "" {
		t.Fatalf("local omlx must register model entries only: %+v", p)
	}
	if !p.hasModel("planner") {
		t.Fatalf("omlx local missing role model: %+v", p)
	}

	// omlx remote: OpenAI protocol at the configured base_url verbatim
	cfg = base()
	cfg.Models["plan"] = ModelProfile{Provider: "omlx", BaseURL: "https://mac-studio:8000/v1", ID: "planner", Thinking: "medium"}
	provs = modelsFromBuild(t, cfg)
	p = provs["omlx-remote-mac-studio:8000"]
	if p.BaseURL != "https://mac-studio:8000/v1" || p.API != "openai-completions" {
		t.Fatalf("omlx remote unexpected: %+v", p)
	}

	// openai-compatible: base_url required, used verbatim
	cfg = base()
	cfg.Models["plan"] = ModelProfile{Provider: "openai-compatible", BaseURL: "http://llm.example.com/v1", ID: "planner", Thinking: "medium"}
	provs = modelsFromBuild(t, cfg)
	p = provs["openai-compatible-llm.example.com"]
	if p.BaseURL != "http://llm.example.com/v1" || p.API != "openai-completions" {
		t.Fatalf("openai-compatible unexpected: %+v", p)
	}
	// default openai keeps the builtin key and default URL
	cfg = base()
	cfg.Models["plan"] = ModelProfile{Provider: "openai", ID: "gpt-4o", Thinking: "off"}
	provs = modelsFromBuild(t, cfg)
	p = provs["openai"]
	if p.BaseURL != "https://api.openai.com/v1" || p.API != "openai-completions" {
		t.Fatalf("openai default unexpected: %+v", p)
	}
	if p.APIKey != "$OPENAI_API_KEY" {
		t.Fatalf("openai default key must reference the provider env var: %q", p.APIKey)
	}

	// anthropic-compatible
	cfg = base()
	cfg.Models["plan"] = ModelProfile{Provider: "anthropic-compatible", BaseURL: "https://gw.example.com/v1", ID: "claude-x", Thinking: "off"}
	provs = modelsFromBuild(t, cfg)
	p = provs["anthropic-compatible-gw.example.com"]
	if p.BaseURL != "https://gw.example.com/v1" || p.API != "anthropic-messages" {
		t.Fatalf("anthropic-compatible unexpected: %+v", p)
	}
}

func TestRegistryIncludesAllowedModels(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	cfg := testConfig(stateDir)
	cfg.AllowedModels = []string{"planner", "coder", "reviewer", "override-only-model"}
	provs := modelsFromBuild(t, cfg)
	p := provs["omlx"]
	if !p.hasModel("override-only-model") {
		t.Fatal("allowed_models entries must be registered so Telegram /use overrides resolve")
	}
	if !p.hasModel("planner") || !p.hasModel("coder") || !p.hasModel("reviewer") {
		t.Fatalf("role models missing from registry: %+v", p)
	}
	// model IDs must be unique per provider
	seen := map[string]bool{}
	for _, m := range p.Models {
		if seen[m.ID] {
			t.Fatalf("duplicate model %q in registry", m.ID)
		}
		seen[m.ID] = true
	}
}

func TestBuildManagedModelsJSONMalformed(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")

	unknown := testConfig(stateDir)
	unknown.Models["plan"] = ModelProfile{Provider: "myservice", ID: "x"}
	if _, err := BuildManagedModelsJSON(unknown); err == nil {
		t.Fatal("unknown provider must be rejected")
	}

	noBase := testConfig(stateDir)
	noBase.Models["plan"] = ModelProfile{Provider: "openai-compatible", ID: "x"}
	if _, err := BuildManagedModelsJSON(noBase); err == nil {
		t.Fatal("openai-compatible without base_url must be rejected")
	}

	noBaseA := testConfig(stateDir)
	noBaseA.Models["plan"] = ModelProfile{Provider: "anthropic-compatible", ID: "x"}
	if _, err := BuildManagedModelsJSON(noBaseA); err == nil {
		t.Fatal("anthropic-compatible without base_url must be rejected")
	}

	emptyID := testConfig(stateDir)
	emptyID.Models["plan"] = ModelProfile{Provider: "omlx", ID: "  "}
	if _, err := BuildManagedModelsJSON(emptyID); err == nil {
		t.Fatal("empty model id must be rejected")
	}

	conflict := testConfig(stateDir)
	conflict.Models["plan"] = ModelProfile{Provider: "openai-compatible", BaseURL: "http://a.example.com/v1", ID: "x", rawAPIKey: "${A_KEY}", APIKey: "a-value"}
	conflict.Models["implement"] = ModelProfile{Provider: "openai-compatible", BaseURL: "http://a.example.com/v1", ID: "y", rawAPIKey: "${B_KEY}", APIKey: "b-value"}
	if _, err := BuildManagedModelsJSON(conflict); err == nil {
		t.Fatal("conflicting credential sources for one endpoint must be rejected")
	}

	// Validation failure must not have written anything: SyncManagedPi on a
	// fresh dir leaves no models.json behind.
	dir := t.TempDir()
	bad := testConfig(dir)
	bad.Models["plan"] = ModelProfile{Provider: "myservice", ID: "x"}
	if err := SyncManagedPi(bad); err == nil {
		t.Fatal("sync of a malformed config must fail")
	}
	if _, err := os.Stat(filepath.Join(dir, "pi", "agent", "models.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("malformed config must not leave a models.json behind")
	}
}

func TestSyncManagedPiSecretSafety(t *testing.T) {
	dir := t.TempDir()
	const secret = "SENTINEL-SECRET-DO-NOT-LEAK"

	// Literal YAML key: must appear only as an env reference in models.json
	// and only in the child environment.
	cfg := testConfig(dir)
	cfg.Models["plan"] = ModelProfile{Provider: "openai-compatible", BaseURL: "http://llm.example.com/v1", ID: "planner", APIKey: secret}
	if err := SyncManagedPi(cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "pi", "agent", "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), secret) {
		t.Fatal("literal api_key leaked into models.json")
	}
	if !strings.Contains(string(data), "${PI_AUTOPILOT_KEY_OPENAI_COMPATIBLE_LLM_EXAMPLE_COM}") {
		t.Fatalf("models.json must reference the child-env key: %s", data)
	}
	env := map[string]string{}
	for _, kv := range PiProcessEnv(cfg) {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if env["PI_AUTOPILOT_KEY_OPENAI_COMPATIBLE_LLM_EXAMPLE_COM"] != secret {
		t.Fatal("literal key must be delivered through the child environment")
	}
	for _, kv := range PiProcessEnv(cfg) {
		if !strings.HasPrefix(kv, "PI_AUTOPILOT_KEY_") && strings.Contains(kv, secret) {
			t.Fatalf("secret outside PI_AUTOPILOT_KEY_* env: %q", kv)
		}
	}

	// ${VAR} reference: preserved verbatim, nothing resolved to disk.
	t.Setenv("SOME_OPENAI_KEY", "env-secret-value")
	dir2 := t.TempDir()
	cfg2 := testConfig(dir2)
	cfg2.Models["plan"] = ModelProfile{Provider: "openai", ID: "gpt-4o", rawAPIKey: "${SOME_OPENAI_KEY}", APIKey: "env-secret-value"}
	if err := SyncManagedPi(cfg2); err != nil {
		t.Fatal(err)
	}
	data2, err := os.ReadFile(filepath.Join(dir2, "pi", "agent", "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data2), "${SOME_OPENAI_KEY}") {
		t.Fatalf("env reference must be preserved in models.json: %s", data2)
	}
	if strings.Contains(string(data2), "env-secret-value") {
		t.Fatal("resolved env value leaked into models.json")
	}
}

func TestSyncManagedPiIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	if err := SyncManagedPi(cfg); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "pi", "agent", "models.json")
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	firstInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Give the clock a chance to tick so a rewrite would be visible.
	time.Sleep(20 * time.Millisecond)
	if err := SyncManagedPi(cfg); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	secondInfo, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("second sync changed the registry bytes")
	}
	if !firstInfo.ModTime().Equal(secondInfo.ModTime()) {
		t.Fatal("second sync rewrote an identical file; setup must be a no-op")
	}
	if _, err := os.Stat(path + ".bak"); err == nil {
		t.Fatal("no-op sync must not create a backup")
	}
}

func TestSyncManagedPiBackupAndRollback(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	agentDir := filepath.Join(dir, "pi", "agent")
	if err := os.MkdirAll(agentDir, 0700); err != nil {
		t.Fatal(err)
	}
	modelsPath := filepath.Join(agentDir, "models.json")
	previous := []byte(`{"providers":{"omlx":{"models":[{"id":"old-model"}]}}}`)
	if err := os.WriteFile(modelsPath, previous, 0600); err != nil {
		t.Fatal(err)
	}
	if err := SyncManagedPi(cfg); err != nil {
		t.Fatal(err)
	}
	bak, err := os.ReadFile(modelsPath + ".bak")
	if err != nil {
		t.Fatal("previous registry must be preserved as models.json.bak")
	}
	if string(bak) != string(previous) {
		t.Fatalf("backup does not hold the previous registry: %s", bak)
	}
	current, err := os.ReadFile(modelsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) == string(previous) {
		t.Fatal("registry was not updated")
	}
	// A validation failure must leave the applied registry untouched.
	bad := testConfig(dir)
	bad.Models["plan"] = ModelProfile{Provider: "myservice", ID: "x"}
	if err := SyncManagedPi(bad); err == nil {
		t.Fatal("malformed sync must fail")
	}
	after, err := os.ReadFile(modelsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(current) {
		t.Fatal("failed sync must not touch the existing registry")
	}
}

func TestSyncManagedPiConflictingLiteralKeys(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	cfg.Models["plan"] = ModelProfile{Provider: "openai-compatible", BaseURL: "http://a.example.com/v1", ID: "x", APIKey: "key-one"}
	cfg.Models["implement"] = ModelProfile{Provider: "openai-compatible", BaseURL: "http://a.example.com/v1", ID: "y", APIKey: "key-two"}
	if err := SyncManagedPi(cfg); err == nil {
		t.Fatal("two different literal keys for one endpoint must be rejected")
	}
}

func TestReadConfigKeepsRawAPIKeyReference(t *testing.T) {
	t.Setenv("OPENAI_API_KEY", "resolved-env-key")
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	source := `repo: owner/repo
state_dir: ./work
models:
  investigate: {provider: ollama, id: a}
  plan: {provider: openai, id: gpt-4o, api_key: "${OPENAI_API_KEY}"}
  implement: {provider: ollama, id: b}
  review: {provider: ollama, id: a}
  chat: {provider: ollama, id: a}
allowed_models: [a, b, gpt-4o]
telegram_chat_id: 123
`
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ReadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Models["plan"].rawAPIKey != "${OPENAI_API_KEY}" {
		t.Fatalf("rawAPIKey must preserve the pre-expansion reference, got %q", cfg.Models["plan"].rawAPIKey)
	}
	if cfg.Models["plan"].APIKey != "resolved-env-key" {
		t.Fatalf("APIKey must still expand for in-process use: %q", cfg.Models["plan"].APIKey)
	}
	data, err := BuildManagedModelsJSON(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "${OPENAI_API_KEY}") {
		t.Fatalf("registry must keep the env reference: %s", data)
	}
	if strings.Contains(string(data), "resolved-env-key") {
		t.Fatal("resolved value leaked into the registry")
	}
}

func TestManagedDoctorMissingModelWarningsAndKeySources(t *testing.T) {
	dir := t.TempDir()
	binDir := t.TempDir()
	// Fake pi that lists only one model: planner is missing for the other
	// roles, so the doctor must warn without panicking or erroring.
	executable := filepath.Join(binDir, "pi")
	script := "#!/bin/sh\nprintf '%s\\n' 'omlx: planner'\n"
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	cfg := testConfig(dir)
	const secret = "SENTINEL-SECRET-DO-NOT-LEAK"
	cfg.Models["plan"] = ModelProfile{Provider: "openai-compatible", BaseURL: "http://llm.example.com/v1", ID: "planner", APIKey: secret}
	out, err := ManagedDoctor(context.Background(), cfg)
	if err != nil {
		t.Fatalf("ManagedDoctor must not fail on missing models, only warn: %v", err)
	}
	if !strings.Contains(out, "WARNING") {
		t.Fatalf("missing role models must produce a WARNING: %s", out)
	}
	if !strings.Contains(out, "managed agent dir: "+filepath.Join(dir, "pi", "agent")) {
		t.Fatalf("doctor must report the managed agent dir: %s", out)
	}
	if !strings.Contains(out, "literal in YAML (child env: PI_AUTOPILOT_KEY_OPENAI_COMPATIBLE_LLM_EXAMPLE_COM)") {
		t.Fatalf("doctor must print key sources: %s", out)
	}
	if strings.Contains(out, secret) {
		t.Fatalf("doctor output leaked the key value: %s", out)
	}
	// The doctor syncs the registry itself.
	if _, err := os.Stat(filepath.Join(dir, "pi", "agent", "models.json")); err != nil {
		t.Fatalf("doctor must ensure the managed registry exists: %v", err)
	}
}

func TestRunPiPassesManagedEnvAndProviderKey(t *testing.T) {
	dir := t.TempDir()
	dumpDir := filepath.Join(dir, "dump")
	if err := os.MkdirAll(dumpDir, 0700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(dir, "pi")
	script := `#!/bin/sh
printf '%s\n' "$@" > "$AUTOPILOT_TEST_DUMP/argv.txt"
env | sort > "$AUTOPILOT_TEST_DUMP/env.txt"
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"ok"}]}}' '{"type":"agent_settled","aborted":false}'
`
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AUTOPILOT_TEST_DUMP", dumpDir)

	stateDir := filepath.Join(dir, "state")
	cfg := testConfig(stateDir)
	const secret = "SENTINEL-SECRET-DO-NOT-LEAK"
	cfg.Models["plan"] = ModelProfile{Provider: "openai-compatible", BaseURL: "http://llm.example.com/v1", ID: "planner", APIKey: secret}
	res := RunPi(context.Background(), cfg, dir, "plan", PiProviderKey(cfg.Models["plan"]), "planner", "medium", "test", filepath.Join(dir, "out.jsonl"), false)
	if res.Err != nil {
		t.Fatalf("session must succeed: %v", res)
	}
	argv, err := os.ReadFile(filepath.Join(dumpDir, "argv.txt"))
	if err != nil {
		t.Fatal(err)
	}
	envText, err := os.ReadFile(filepath.Join(dumpDir, "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	argvText := string(argv)
	if !strings.Contains(argvText, "openai-compatible-llm.example.com") {
		t.Fatalf("pi must be launched with the registry provider key: %s", argvText)
	}
	if !strings.Contains(argvText, "--no-session") {
		t.Fatalf("fresh session flags must survive: %s", argvText)
	}
	if strings.Contains(argvText, secret) {
		t.Fatalf("secret must never appear in argv: %s", argvText)
	}
	envLines := string(envText)
	if !strings.Contains(envLines, "PI_CODING_AGENT_DIR="+filepath.Join(stateDir, "pi", "agent")) {
		t.Fatalf("managed agent dir missing from child env: %s", envLines)
	}
	if !strings.Contains(envLines, "PI_CODING_AGENT_SESSION_DIR="+filepath.Join(stateDir, "pi", "sessions")) {
		t.Fatalf("managed session dir missing from child env: %s", envLines)
	}
	if !strings.Contains(envLines, "PI_AUTOPILOT_KEY_OPENAI_COMPATIBLE_LLM_EXAMPLE_COM="+secret) {
		t.Fatal("literal key must reach the child environment exactly once as PI_AUTOPILOT_KEY_*")
	}
	if strings.Count(envLines, secret) != 1 {
		t.Fatalf("secret must appear exactly once in child env: %s", envLines)
	}
	// Inherited environment must survive the merge.
	if !strings.Contains(envLines, "AUTOPILOT_TEST_DUMP="+dumpDir) {
		t.Fatal("inherited environment variables must be preserved")
	}
}

func TestSetupSubcommandPipelineIdempotent(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	if err := SyncManagedPi(cfg); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(filepath.Join(dir, "pi", "agent", "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	if err := SyncManagedPi(cfg); err != nil {
		t.Fatal(err)
	}
	second, err := os.ReadFile(filepath.Join(dir, "pi", "agent", "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatal("setup pipeline must be byte-identical on re-run")
	}
}
