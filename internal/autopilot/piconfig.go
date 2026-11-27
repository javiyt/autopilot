package autopilot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Managed Pi installation (issue #4).
//
// The supervisor runs every Pi session against a *managed* agent directory
// (PI_CODING_AGENT_DIR) inside the supervisor's state tree, so the user's
// global ~/.pi/agent is never read or written. The managed directory holds
// only models.json: a provider registry generated from config.yaml. Secrets
// never land in the file: models.json carries environment references
// ($NAME / ${NAME}) that Pi interpolates at request time, and literal YAML
// keys travel only in the child process environment as PI_AUTOPILOT_KEY_*.
// This file must never log a credential value; print sources only.

const piModelsSchema = "https://pi.dev/schemas/models.schema.json"

type piModel struct {
	ID string `json:"id"`
}

type piProvider struct {
	BaseURL string    `json:"baseUrl,omitempty"`
	API     string    `json:"api,omitempty"` // "openai-completions" | "anthropic-messages"
	APIKey  string    `json:"apiKey,omitempty"`
	Models  []piModel `json:"models"`
}

type piModelsFile struct {
	Schema    string                `json:"$schema"`
	Providers map[string]piProvider `json:"providers"`
}

var knownProviders = map[string]bool{
	"omlx":                 true,
	"ollama":               true,
	"openai":               true,
	"openai-compatible":    true,
	"anthropic":            true,
	"anthropic-compatible": true,
}

// ManagedPiDirs returns the managed agent directory (models.json) and the
// managed session directory for Pi child processes. PiAgentDir, when set, is
// resolved relative to StateDir unless absolute.
func ManagedPiDirs(cfg Config) (agentDir, sessionDir string) {
	if cfg.PiAgentDir != "" {
		if filepath.IsAbs(cfg.PiAgentDir) {
			agentDir = cfg.PiAgentDir
		} else {
			agentDir = filepath.Join(cfg.StateDir, cfg.PiAgentDir)
		}
	} else {
		agentDir = filepath.Join(cfg.StateDir, "pi", "agent")
	}
	sessionDir = filepath.Join(cfg.StateDir, "pi", "sessions")
	return agentDir, sessionDir
}

func piProviderDefaultBaseURL(provider string) string {
	switch provider {
	case "omlx":
		return ""
	case "ollama":
		return "http://localhost:11434"
	case "openai", "openai-compatible":
		return "https://api.openai.com/v1"
	case "anthropic", "anthropic-compatible":
		return "https://api.anthropic.com/v1"
	}
	return ""
}

// endpointURL returns the (baseURL, api) pair a profile maps to, applying
// the provider defaults and normalization that BuildManagedModelsJSON writes
// into models.json. It is the single source of truth shared with the remote
// probe (remote.go): doctor and the stage pre-flight must hit exactly the
// URL Pi would call, never a differently-normalized one.
func endpointURL(baseURL, provider string) (string, string) {
	switch provider {
	case "omlx":
		// Local omlx is a built-in provider (empty URL); a remote omlx
		// speaks the OpenAI protocol at its base_url verbatim.
		if baseURL == "" {
			return "", ""
		}
		return baseURL, "openai-completions"
	case "ollama":
		if baseURL == "" {
			baseURL = piProviderDefaultBaseURL("ollama")
		}
		if !strings.HasSuffix(baseURL, "/v1") {
			baseURL += "/v1"
		}
		return baseURL, "openai-completions"
	case "openai":
		if baseURL == "" {
			baseURL = piProviderDefaultBaseURL("openai")
		}
		return baseURL, "openai-completions"
	case "openai-compatible":
		return baseURL, "openai-completions"
	case "anthropic":
		if baseURL == "" {
			baseURL = piProviderDefaultBaseURL("anthropic")
		}
		return baseURL, "anthropic-messages"
	case "anthropic-compatible":
		return baseURL, "anthropic-messages"
	}
	return baseURL, ""
}

// PiProviderKey is the single source of truth for the `pi --provider` value
// and the provider key under which the model is registered in the managed
// models.json. A non-default endpoint gets a host-suffixed custom key so two
// endpoints behind the same protocol never collide in the registry.
func PiProviderKey(p ModelProfile) string {
	switch p.Provider {
	case "omlx":
		if p.BaseURL == "" {
			return "omlx"
		}
		return "omlx-remote-" + hostPortOf(p.BaseURL)
	case "ollama":
		return "ollama"
	case "openai", "openai-compatible", "anthropic", "anthropic-compatible":
		if p.BaseURL == "" || p.BaseURL == piProviderDefaultBaseURL(p.Provider) {
			return p.Provider
		}
		return p.Provider + "-" + hostPortOf(p.BaseURL)
	default:
		return p.Provider
	}
}

func hostPortOf(baseURL string) string {
	if u, err := url.Parse(baseURL); err == nil && u.Host != "" {
		return u.Host
	}
	s := baseURL
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	return s
}

var piEnvRefPattern = regexp.MustCompile(`^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$`)

// piKeyEnvVarName derives the child-environment variable that carries a
// literal YAML api_key: PI_AUTOPILOT_KEY_<sanitized provider key>.
func piKeyEnvVarName(providerKey string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(providerKey) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	return "PI_AUTOPILOT_KEY_" + b.String()
}

// piCredential describes where one provider's API key comes from. Ref is the
// value written to models.json: an environment reference ($NAME / ${NAME}),
// a dummy key, or empty (no key). EnvVar/EnvValue carry a literal key only
// through the child process environment, never argv or disk. Source is
// doctor-facing text and must never include the key value.
type piCredential struct {
	Ref      string
	EnvVar   string
	EnvValue string
	Source   string
}

func piCredentialFor(p ModelProfile, providerKey string) piCredential {
	// rawAPIKey is the pre-expansion YAML value captured by ReadConfig; when
	// it was not captured (programmatic configs) fall back to the current
	// value so a set key is still treated as literal.
	raw := p.rawAPIKey
	if raw == "" {
		raw = p.APIKey
	}
	if m := piEnvRefPattern.FindStringSubmatch(raw); m != nil {
		return piCredential{Ref: "${" + m[1] + "}", Source: "env: " + m[1]}
	}
	if raw != "" {
		name := piKeyEnvVarName(providerKey)
		return piCredential{Ref: "${" + name + "}", EnvVar: name, EnvValue: p.APIKey, Source: "literal in YAML (child env: " + name + ")"}
	}
	switch p.Provider {
	case "openai", "openai-compatible":
		return piCredential{Ref: "$OPENAI_API_KEY", Source: "env: OPENAI_API_KEY (Pi default)"}
	case "anthropic", "anthropic-compatible":
		return piCredential{Ref: "$ANTHROPIC_API_KEY", Source: "env: ANTHROPIC_API_KEY (Pi default)"}
	case "ollama":
		return piCredential{Ref: "ollama", Source: "none (local, dummy key)"}
	default: // omlx and other local endpoints
		return piCredential{Source: "none (local)"}
	}
}

// effectiveVar is the environment variable that must resolve for the key, or
// "" for dummy/absent keys. Conflicting sources for one provider key are a
// config error: one endpoint cannot carry two different credentials.
func (c piCredential) effectiveVar() string {
	if c.EnvVar != "" {
		return c.EnvVar
	}
	if m := piEnvRefPattern.FindStringSubmatch(c.Ref); m != nil {
		return m[1]
	}
	switch c.Ref {
	case "$OPENAI_API_KEY":
		return "OPENAI_API_KEY"
	case "$ANTHROPIC_API_KEY":
		return "ANTHROPIC_API_KEY"
	}
	return ""
}

func resolvePiCredentials(cfg Config) (map[string]piCredential, error) {
	creds := map[string]piCredential{}
	for _, role := range sortedModelRoles(cfg) {
		p := cfg.Models[role]
		key := PiProviderKey(p)
		cred := piCredentialFor(p, key)
		prev, ok := creds[key]
		if !ok {
			creds[key] = cred
			continue
		}
		if prev.effectiveVar() != cred.effectiveVar() {
			return nil, fmt.Errorf("provider endpoint %q has conflicting credential sources: %s vs %s (models.%s)", key, prev.Source, cred.Source, role)
		}
		if prev.EnvValue != "" && cred.EnvValue != "" && prev.EnvValue != cred.EnvValue {
			return nil, fmt.Errorf("provider endpoint %q has conflicting literal api_key values (models.%s)", key, role)
		}
	}
	return creds, nil
}

func sortedModelRoles(cfg Config) []string {
	roles := make([]string, 0, len(cfg.Models))
	for role := range cfg.Models {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	return roles
}

// BuildManagedModelsJSON renders the managed models.json from the config.
// It validates the document before returning; callers must treat any error
// as "do not touch the on-disk registry".
func BuildManagedModelsJSON(cfg Config) ([]byte, error) {
	creds, err := resolvePiCredentials(cfg)
	if err != nil {
		return nil, err
	}
	providers := map[string]*piProvider{}
	ensure := func(key string) *piProvider {
		prov := providers[key]
		if prov == nil {
			prov = &piProvider{}
			providers[key] = prov
		}
		return prov
	}
	addModel := func(key, id string) {
		prov := providers[key]
		if prov == nil {
			return
		}
		for _, m := range prov.Models {
			if m.ID == id {
				return
			}
		}
		prov.Models = append(prov.Models, piModel{ID: id})
	}

	roles := sortedModelRoles(cfg)
	roleIDs := map[string]bool{}
	for _, role := range roles {
		p := cfg.Models[role]
		if p.Provider == "" {
			return nil, fmt.Errorf("models.%s.provider is required", role)
		}
		if !knownProviders[p.Provider] {
			return nil, fmt.Errorf("models.%s.provider %q is unknown (known: omlx, ollama, openai, openai-compatible, anthropic, anthropic-compatible)", role, p.Provider)
		}
		if strings.TrimSpace(p.ID) == "" {
			return nil, fmt.Errorf("models.%s.id is required", role)
		}
		key := PiProviderKey(p)
		prov := ensure(key)
		baseURL, api := endpointURL(p.BaseURL, p.Provider)
		switch p.Provider {
		case "openai-compatible", "anthropic-compatible":
			if p.BaseURL == "" {
				return nil, fmt.Errorf("models.%s.base_url is required for provider %s", role, p.Provider)
			}
		case "ollama":
			if prov.BaseURL != "" && prov.BaseURL != baseURL {
				return nil, fmt.Errorf("models.%s: provider ollama already registered with a different base_url", role)
			}
		}
		if baseURL != "" {
			prov.BaseURL, prov.API = baseURL, api
		}
		if cred := creds[key]; cred.Ref != "" {
			prov.APIKey = cred.Ref
		}
		roleIDs[p.ID] = true
		addModel(key, p.ID)
	}

	// allowed_models that no role profile names are registered under every
	// configured provider so a Telegram /use override always resolves inside
	// the isolated registry; /use keeps the role's provider and endpoint.
	for _, m := range cfg.AllowedModels {
		if roleIDs[m] {
			continue
		}
		for key := range providers {
			addModel(key, m)
		}
	}

	for _, prov := range providers {
		sort.Slice(prov.Models, func(i, j int) bool { return prov.Models[i].ID < prov.Models[j].ID })
	}
	if len(providers) == 0 {
		return nil, errors.New("no model profiles to register in the managed Pi registry")
	}
	// Post-build validation: every role model must resolve under its provider.
	for _, role := range roles {
		p := cfg.Models[role]
		key := PiProviderKey(p)
		if !providers[key].hasModel(p.ID) {
			return nil, fmt.Errorf("model %q (models.%s) did not resolve under provider %q", p.ID, role, key)
		}
	}
	doc := piModelsFile{Schema: piModelsSchema, Providers: make(map[string]piProvider, len(providers))}
	for key, prov := range providers {
		doc.Providers[key] = *prov
	}
	return json.MarshalIndent(doc, "", "  ")
}

func (p *piProvider) hasModel(id string) bool {
	for _, m := range p.Models {
		if m.ID == id {
			return true
		}
	}
	return false
}

// SyncManagedPi regenerates the managed agent directory and models.json from
// the config. It is idempotent: when the generated bytes match the on-disk
// registry nothing is written. When they differ, the previous registry is
// first preserved atomically as models.json.bak (rollback), then the new one
// replaces it atomically. A validation error leaves the existing file
// untouched.
func SyncManagedPi(cfg Config) error {
	data, err := BuildManagedModelsJSON(cfg)
	if err != nil {
		return err
	}
	agentDir, _ := ManagedPiDirs(cfg)
	if err := os.MkdirAll(agentDir, 0700); err != nil {
		return err
	}
	modelsPath := filepath.Join(agentDir, "models.json")
	existing, err := os.ReadFile(modelsPath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err == nil && bytes.Equal(existing, data) {
		return nil // idempotent: no visible mutation
	}
	if err == nil && len(existing) > 0 {
		if err := atomicBytes(modelsPath+".bak", existing); err != nil {
			return fmt.Errorf("backup previous models.json: %w", err)
		}
	}
	return atomicBytes(modelsPath, data)
}

// PiProcessEnv is the managed environment for Pi child processes: the agent
// and session directories plus every literal YAML key as PI_AUTOPILOT_KEY_*.
// It never contains a credential in an argv-visible form.
func PiProcessEnv(cfg Config) []string {
	agentDir, sessionDir := ManagedPiDirs(cfg)
	env := []string{
		"PI_CODING_AGENT_DIR=" + agentDir,
		"PI_CODING_AGENT_SESSION_DIR=" + sessionDir,
	}
	if creds, err := resolvePiCredentials(cfg); err == nil {
		keys := make([]string, 0, len(creds))
		for key := range creds {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			if cred := creds[key]; cred.EnvVar != "" {
				env = append(env, cred.EnvVar+"="+cred.EnvValue)
			}
		}
	}
	return env
}

// piChildEnv merges the managed variables over the inherited environment:
// managed values win, everything else (PATH, HOME, provider default keys) is
// preserved so Pi and its child tools keep working.
func piChildEnv(cfg Config) []string {
	managed := PiProcessEnv(cfg)
	managedNames := map[string]bool{}
	for _, kv := range managed {
		if i := strings.IndexByte(kv, '='); i >= 0 {
			managedNames[kv[:i]] = true
		}
	}
	out := make([]string, 0, len(managed)+len(os.Environ()))
	out = append(out, managed...)
	for _, kv := range os.Environ() {
		if i := strings.IndexByte(kv, '='); i >= 0 && managedNames[kv[:i]] {
			continue
		}
		out = append(out, kv)
	}
	return out
}
