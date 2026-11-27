package autopilot

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// Remote endpoint probing (issue #5).
//
// The probe hits exactly the URL the managed Pi registry was generated from
// (endpointURL mirrors BuildManagedModelsJSON), so doctor and the stage
// pre-flight can never disagree with what Pi would actually call. The
// configured key is sent only to the user-declared endpoint; a key value
// must never be interpolated into any Message here (repo invariant).

// RemoteStatus classifies the outcome of probing one remote inference endpoint.
type RemoteStatus string

const (
	StatusOK            RemoteStatus = "ok"
	StatusAuthFailed    RemoteStatus = "auth-failed"
	StatusModelNotFound RemoteStatus = "model-not-found"
	StatusUnreachable   RemoteStatus = "unreachable"
)

// RemoteCheckResult is the outcome of probing one provider endpoint.
// Message is actionable and human-readable; it NEVER contains a key value.
type RemoteCheckResult struct {
	ProviderKey string   // registry key, e.g. "omlx-remote-mac-studio:8000"
	Roles       []string // sorted roles served by this endpoint
	Status      RemoteStatus
	Message     string
}

// remoteClient is the probe client: bounded timeout, no retries. One cheap
// GET /models per endpoint keeps doctor and pre-flight cost low on a LAN.
func remoteClient() *http.Client {
	return &http.Client{Timeout: 10 * time.Second}
}

// CheckRemoteEndpoints probes every remote endpoint in cfg over HTTP and
// returns one result per provider key (roles sharing an endpoint probe once).
// Local providers are skipped: omlx without base_url is a macOS built-in and
// ollama targets a local runtime by design. client may be nil, in which case
// a 10 s timeout client is used.
func CheckRemoteEndpoints(ctx context.Context, client *http.Client, cfg Config) []RemoteCheckResult {
	if client == nil {
		client = remoteClient()
	}
	type entry struct {
		role    string
		profile ModelProfile
	}
	byKey := map[string][]entry{}
	var keys []string
	for _, role := range sortedModelRoles(cfg) {
		p := cfg.Models[role]
		if p.Provider == "" {
			continue
		}
		if !probeableProfile(p) {
			continue
		}
		key := PiProviderKey(p)
		if _, ok := byKey[key]; !ok {
			keys = append(keys, key)
		}
		byKey[key] = append(byKey[key], entry{role: role, profile: p})
	}
	sort.Strings(keys)
	results := make([]RemoteCheckResult, 0, len(keys))
	for _, key := range keys {
		entries := byKey[key]
		ids := make([]string, 0, len(entries))
		roles := make([]string, 0, len(entries))
		profile := entries[0].profile
		for _, e := range entries {
			roles = append(roles, e.role)
			if e.profile.ID != "" {
				ids = append(ids, e.profile.ID)
			}
		}
		res := probeEndpoint(ctx, client, profile, key, roles, ids)
		results = append(results, res)
	}
	return results
}

// probeableProfile reports whether a profile targets a remote endpoint the
// probe should check: non-empty effective base URL, not ollama (local by
// design, never probed), not local omlx (built-in macOS provider).
func probeableProfile(p ModelProfile) bool {
	if p.Provider == "ollama" {
		return false
	}
	baseURL, _ := endpointURL(p.BaseURL, p.Provider)
	return baseURL != ""
}

// probeEndpoint probes one endpoint (GET {base}/models) and classifies the
// outcome. ids are the model IDs every role under this endpoint expects;
// ok requires all of them to be present in the catalog.
func probeEndpoint(ctx context.Context, client *http.Client, p ModelProfile, providerKey string, roles, ids []string) RemoteCheckResult {
	res := RemoteCheckResult{ProviderKey: providerKey, Roles: roles}
	base, api := endpointURL(p.BaseURL, p.Provider)
	finish := func(status RemoteStatus, message string) RemoteCheckResult {
		res.Status = status
		res.Message = message
		if status == StatusOK && isUnencryptedRemote(base) {
			res.Message += " [WARNING: unencrypted http — use Tailscale or an https reverse proxy]"
		}
		return res
	}
	if base == "" {
		return finish(StatusUnreachable, "no effective endpoint URL")
	}
	probeURL := strings.TrimSuffix(base, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, probeURL, nil)
	if err != nil {
		return finish(StatusUnreachable, "invalid endpoint URL: "+err.Error())
	}
	key := probeKey(p, providerKey)
	if api == "anthropic-messages" {
		if key != "" {
			req.Header.Set("x-api-key", key)
		}
	} else if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := client.Do(req)
	if err != nil {
		host := hostPortOf(base)
		return finish(StatusUnreachable, fmt.Sprintf("endpoint %s unreachable — check the LAN route, firewall, that the inference process is bound on %s, and TLS settings (%v)", host, host, err))
	}
	defer resp.Body.Close()

	host := hostPortOf(base)
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		msg := fmt.Sprintf("credential rejected by %s (HTTP %d)", host, resp.StatusCode)
		if v := probeCredentialVar(p, providerKey); v != "" {
			msg += "; check that " + v + " is set"
		}
		return finish(StatusAuthFailed, msg)
	case resp.StatusCode == http.StatusNotFound:
		// Some OpenAI-compatible servers (and older mlx_server builds) do
		// not expose GET /models. The endpoint may still serve chat; doctor
		// treats this as a catalog miss (WARNING) rather than a hard failure,
		// and the stage pre-flight hard-fails only on connect/auth errors.
		return finish(StatusModelNotFound, "GET /models returned 404 — catalog not exposed; the endpoint may still work")
	case resp.StatusCode != http.StatusOK:
		return finish(StatusUnreachable, fmt.Sprintf("endpoint %s returned HTTP %d — check the inference server configuration", host, resp.StatusCode))
	}

	var catalog struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
		return finish(StatusModelNotFound, "could not parse the model catalog: "+err.Error())
	}
	available := make(map[string]bool, len(catalog.Data))
	var availableList []string
	for _, m := range catalog.Data {
		available[m.ID] = true
		availableList = append(availableList, m.ID)
	}
	sort.Strings(availableList)
	var missing []string
	for _, id := range ids {
		if !available[id] {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		sample := availableList
		if len(sample) > 3 {
			sample = sample[:3]
		}
		return finish(StatusModelNotFound, fmt.Sprintf("model(s) %s not in catalog at %s (available: %s)", strings.Join(missing, ", "), host, strings.Join(sample, ", ")))
	}
	return finish(StatusOK, fmt.Sprintf("endpoint %s reachable (model %s served)", host, strings.Join(ids, ", ")))
}

// probeKey resolves the key value for the probe using the same credential
// rules as piCredentialFor: literal YAML keys travel through the child env
// (PI_AUTOPILOT_KEY_*) and are read from the profile here; ${VAR}
// references and provider defaults are resolved from the process environment.
func probeKey(p ModelProfile, providerKey string) string {
	cred := piCredentialFor(p, providerKey)
	if cred.EnvValue != "" {
		return cred.EnvValue
	}
	if v := cred.effectiveVar(); v != "" {
		return os.Getenv(v)
	}
	return ""
}

// probeCredentialVar is the environment variable name an operator must check
// when authentication fails; never the key value itself.
func probeCredentialVar(p ModelProfile, providerKey string) string {
	cred := piCredentialFor(p, providerKey)
	if m := piEnvRefPattern.FindStringSubmatch(cred.Ref); m != nil {
		return "${" + m[1] + "}"
	}
	if v := cred.effectiveVar(); v != "" && v != "PI_AUTOPILOT_KEY_" {
		if strings.HasPrefix(v, "PI_AUTOPILOT_KEY_") {
			return "the literal api_key in config.yaml"
		}
		return v
	}
	return ""
}

// isUnencryptedRemote reports whether the base URL is plain http to a
// non-localhost host; those deserve a visible transport warning.
func isUnencryptedRemote(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme != "http" {
		return false
	}
	host := u.Hostname()
	return host != "localhost" && host != "127.0.0.1" && host != "::1"
}
