package autopilot

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// endpointURL mirrors what BuildManagedModelsJSON writes into models.json, so
// the probe always hits the exact URL Pi would call.
func TestEndpointURLMatchesRegistry(t *testing.T) {
	cases := []struct {
		provider, baseURL, wantBase, wantAPI string
	}{
		{"omlx", "", "", ""},                              // built-in local
		{"omlx", "https://mac-studio:8000/v1", "https://mac-studio:8000/v1", "openai-completions"},
		{"ollama", "", "http://localhost:11434/v1", "openai-completions"},
		{"ollama", "http://host:11434", "http://host:11434/v1", "openai-completions"},
		{"ollama", "http://host:11434/v1", "http://host:11434/v1", "openai-completions"},
		{"openai", "", "https://api.openai.com/v1", "openai-completions"},
		{"openai-compatible", "http://llm.example.com/v1", "http://llm.example.com/v1", "openai-completions"},
		{"anthropic", "", "https://api.anthropic.com/v1", "anthropic-messages"},
		{"anthropic-compatible", "https://gw.example.com/v1", "https://gw.example.com/v1", "anthropic-messages"},
	}
	for _, c := range cases {
		base, api := endpointURL(c.baseURL, c.provider)
		if base != c.wantBase || api != c.wantAPI {
			t.Errorf("endpointURL(%q, %q) = (%q, %q), want (%q, %q)", c.provider, c.baseURL, base, api, c.wantBase, c.wantAPI)
		}
	}
}

// modelsServer stands up an httptest OpenAI-compatible server. ids is the
// advertised catalog; wantAuth/wantXAPIKey record the header the probe must send.
func modelsServer(t *testing.T, ids []string, code int, gotAuth *string, gotXAPIKey *string, gotPath *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if gotAuth != nil {
			*gotAuth = r.Header.Get("Authorization")
		}
		if gotXAPIKey != nil {
			*gotXAPIKey = r.Header.Get("x-api-key")
		}
		if gotPath != nil {
			*gotPath = r.URL.Path
		}
		if code != http.StatusOK {
			w.WriteHeader(code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var b strings.Builder
		b.WriteString(`{"data":[`)
		for i, id := range ids {
			if i > 0 {
				b.WriteString(",")
			}
			fmt.Fprintf(&b, `{"id":%q}`, id)
		}
		b.WriteString(`]}`)
		fmt.Fprint(w, b.String())
	}))
}

func remoteTestConfig(stateDir string, profile ModelProfile) Config {
	cfg := testConfig(stateDir)
	for role := range cfg.Models {
		cfg.Models[role] = profile
	}
	cfg.AllowedModels = []string{profile.ID}
	return cfg
}

func TestCheckRemoteEndpointsReachableModelPresent(t *testing.T) {
	const secret = "SENTINEL-SECRET-DO-NOT-LEAK"
	var auth, xapiKey, path string
	ts := modelsServer(t, []string{"planner"}, http.StatusOK, &auth, &xapiKey, &path)
	defer ts.Close()

	stateDir := t.TempDir()
	cfg := remoteTestConfig(stateDir, ModelProfile{Provider: "openai-compatible", BaseURL: ts.URL + "/v1", ID: "planner", APIKey: secret})
	results := CheckRemoteEndpoints(context.Background(), nil, cfg)
	if len(results) != 1 {
		t.Fatalf("want 1 result (roles share one endpoint), got %d: %+v", len(results), results)
	}
	res := results[0]
	if res.Status != StatusOK {
		t.Fatalf("status = %q, want ok: %s", res.Status, res.Message)
	}
	if path != "/v1/models" {
		t.Fatalf("probe must hit {base}/models, got path %q", path)
	}
	if auth != "Bearer "+secret {
		t.Fatalf("OpenAI family must send Authorization: Bearer, got %q", auth)
	}
	if xapiKey != "" {
		t.Fatalf("OpenAI family must not send x-api-key, got %q", xapiKey)
	}
	// All five testConfig roles share the endpoint: deduped to one probe.
	if len(res.Roles) != 5 {
		t.Fatalf("deduped result must list all roles served by the endpoint, got %v", res.Roles)
	}
	for _, out := range []string{res.Message, res.ProviderKey} {
		if strings.Contains(out, secret) {
			t.Fatalf("key value leaked into probe output: %q", out)
		}
	}
}

func TestCheckRemoteEndpointsAuthFailed(t *testing.T) {
	t.Setenv("OMLX_PROBE_KEY", "env-key-value")
	var auth string
	ts := modelsServer(t, nil, http.StatusUnauthorized, &auth, nil, nil)
	defer ts.Close()

	stateDir := t.TempDir()
	cfg := remoteTestConfig(stateDir, ModelProfile{Provider: "openai-compatible", BaseURL: ts.URL + "/v1", ID: "planner", rawAPIKey: "${OMLX_PROBE_KEY}", APIKey: "env-key-value"})
	results := CheckRemoteEndpoints(context.Background(), nil, cfg)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	res := results[0]
	if res.Status != StatusAuthFailed {
		t.Fatalf("status = %q, want auth-failed: %s", res.Status, res.Message)
	}
	if auth != "Bearer env-key-value" {
		t.Fatalf("probe must send the resolved env key, got %q", auth)
	}
	if !strings.Contains(res.Message, "OMLX_PROBE_KEY") {
		t.Fatalf("auth-failed message must name the env var to check: %s", res.Message)
	}
	if strings.Contains(res.Message, "env-key-value") {
		t.Fatalf("key value leaked into message: %s", res.Message)
	}
}

func TestCheckRemoteEndpointsModelNotFound(t *testing.T) {
	ts := modelsServer(t, []string{"other-1", "other-2", "other-3", "other-4"}, http.StatusOK, nil, nil, nil)
	defer ts.Close()

	stateDir := t.TempDir()
	cfg := remoteTestConfig(stateDir, ModelProfile{Provider: "openai-compatible", BaseURL: ts.URL + "/v1", ID: "planner"})
	results := CheckRemoteEndpoints(context.Background(), nil, cfg)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	res := results[0]
	if res.Status != StatusModelNotFound {
		t.Fatalf("status = %q, want model-not-found: %s", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "planner") {
		t.Fatalf("message must name the missing model: %s", res.Message)
	}
	// Catalog sample is capped at 3 IDs.
	if strings.Contains(res.Message, "other-4") {
		t.Fatalf("available-model sample must cap at 3 IDs: %s", res.Message)
	}
}

func TestCheckRemoteEndpointsUnreachable(t *testing.T) {
	// Bind a server, then close it to obtain a real dead port on localhost.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	stateDir := t.TempDir()
	cfg := remoteTestConfig(stateDir, ModelProfile{Provider: "omlx", BaseURL: deadURL + "/v1", ID: "planner"})
	results := CheckRemoteEndpoints(context.Background(), nil, cfg)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	res := results[0]
	if res.Status != StatusUnreachable {
		t.Fatalf("status = %q, want unreachable: %s", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "unreachable") {
		t.Fatalf("message must be actionable about reachability: %s", res.Message)
	}
}

func TestCheckRemoteEndpointsCatalog404(t *testing.T) {
	// A server that exists but does not expose GET /models (older mlx_server).
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer ts.Close()

	stateDir := t.TempDir()
	cfg := remoteTestConfig(stateDir, ModelProfile{Provider: "omlx", BaseURL: ts.URL + "/v1", ID: "planner"})
	results := CheckRemoteEndpoints(context.Background(), nil, cfg)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	res := results[0]
	if res.Status != StatusModelNotFound {
		t.Fatalf("404 on /models must classify as model-not-found (catalog miss), got %q: %s", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "404") {
		t.Fatalf("message must explain the missing catalog: %s", res.Message)
	}
}

func TestCheckRemoteEndpointsAnthropicHeader(t *testing.T) {
	const secret = "SENTINEL-ANTHROPIC-SECRET"
	var auth, xapiKey string
	ts := modelsServer(t, []string{"claude-x"}, http.StatusOK, &auth, &xapiKey, nil)
	defer ts.Close()

	stateDir := t.TempDir()
	cfg := remoteTestConfig(stateDir, ModelProfile{Provider: "anthropic-compatible", BaseURL: ts.URL + "/v1", ID: "claude-x", APIKey: secret})
	results := CheckRemoteEndpoints(context.Background(), nil, cfg)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if results[0].Status != StatusOK {
		t.Fatalf("status = %q, want ok: %s", results[0].Status, results[0].Message)
	}
	if xapiKey != secret {
		t.Fatalf("anthropic family must send x-api-key, got %q", xapiKey)
	}
	if auth != "" {
		t.Fatalf("anthropic family must not send Authorization, got %q", auth)
	}
}

func TestCheckRemoteEndpointsSkipsLocalProviders(t *testing.T) {
	stateDir := t.TempDir()
	// Local omlx (built-in) and ollama must not be probed at all.
	cfg := testConfig(stateDir)
	// testConfig is all local omlx; add an ollama role too.
	cfg.Models["chat"] = ModelProfile{Provider: "ollama", ID: "planner", Thinking: "off"}
	cfg.AllowedModels = []string{"planner", "coder", "reviewer"}
	results := CheckRemoteEndpoints(context.Background(), nil, cfg)
	if len(results) != 0 {
		t.Fatalf("local providers must not be probed, got %+v", results)
	}
}

func TestCheckRemoteEndpointsUnencryptedWarning(t *testing.T) {
	// A non-localhost http URL: stand up a server on 127.0.0.1 is impossible
	// with httptest (it binds 127.0.0.1), so assert the warning logic directly.
	if !isUnencryptedRemote("http://mac-studio.example:8000/v1") {
		t.Fatal("off-LAN http must be flagged as unencrypted")
	}
	if isUnencryptedRemote("https://mac-studio.example:8000/v1") {
		t.Fatal("https must not be flagged")
	}
	if isUnencryptedRemote("http://localhost:8000/v1") {
		t.Fatal("localhost http must not be flagged")
	}
	if isUnencryptedRemote("http://127.0.0.1:8000/v1") {
		t.Fatal("127.0.0.1 http must not be flagged")
	}
}
