package autopilot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type ModelProfile struct {
	Provider string `yaml:"provider"`
	ID       string `yaml:"id"`
	Thinking string `yaml:"thinking"`
	// For external providers
	BaseURL string `yaml:"base_url"` // Compatible endpoint URL (Ollama, OpenAI, Anthropic-compatible)
	APIKey  string `yaml:"api_key"`  // API key if authentication required
	// rawAPIKey preserves the pre-expansion YAML value so the managed Pi
	// registry can tell ${VAR} references (kept as env references) from
	// literal keys (moved to child env). Set by ReadConfig before expansion;
	// no YAML/JSON tags on purpose.
	rawAPIKey string
}

type Config struct {
	Repo      string `yaml:"repo"`
	Milestone string `yaml:"milestone"`
	Label     string `yaml:"label"`
	StateDir  string `yaml:"state_dir"`
	// PiAgentDir locates the managed Pi agent directory (models.json); when
	// set relative it resolves under StateDir. Default: <state_dir>/pi/agent.
	PiAgentDir string `yaml:"pi_agent_dir"`
	// Path is set by ReadConfig to the absolute configuration file location.
	Path                string
	PollSeconds         int                     `yaml:"poll_seconds"`
	IdleTimeoutMinutes  int                     `yaml:"idle_timeout_minutes"`
	StageTimeoutMinutes int                     `yaml:"stage_timeout_minutes"`
	MaxStageRetries     int                     `yaml:"max_stage_retries"`
	MaxReviewRounds     int                     `yaml:"max_review_rounds"`
	MaxCIFixes          int                     `yaml:"max_ci_fixes"`
	MaxLocalFixes       int                     `yaml:"max_local_fixes"`
	MaxBaseSyncRounds   int                     `yaml:"max_base_sync_rounds"`
	CIWaitMinutes       int                     `yaml:"ci_wait_minutes"`
	AutoMerge           bool                    `yaml:"auto_merge"`
	CheckCommand        string                  `yaml:"check_command"`
	CheckCommands       []string                `yaml:"check_commands"`
	CIRerunPatterns     []string                `yaml:"ci_rerun_patterns"`
	ProjectNumber       int                     `yaml:"project_number"`
	ProjectOwner        string                  `yaml:"project_owner"`
	ProjectInProgress   string                  `yaml:"project_in_progress"`
	Models              map[string]ModelProfile `yaml:"models"`
	AllowedModels       []string                `yaml:"allowed_models"`
	TelegramChatID      int64                   `yaml:"telegram_chat_id"`
	AuthorizedUserID    int64                   `yaml:"authorized_user_id,omitempty"`
	ManualRepo          string                  `yaml:"manual_repo,omitempty"`
	ManualMilestone     string                  `yaml:"manual_milestone,omitempty"`
}

var repoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

func ReadConfig(path string) (Config, error) {
	var cfg Config
	data, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if !strings.HasSuffix(path, ".yaml") && !strings.HasSuffix(path, ".yml") {
		return cfg, errors.New("configuration must be a .yaml or .yml file")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err = decoder.Decode(&cfg); err != nil {
		return cfg, fmt.Errorf("invalid config: %w", err)
	}

	// Preserve the pre-expansion api_key so the managed Pi registry can keep
	// ${VAR} references as env references instead of inlined values.
	for role, profile := range cfg.Models {
		profile.rawAPIKey = profile.APIKey
		cfg.Models[role] = profile
	}

	// Expand environment variables before validation
	cfg = cfg.ExpandEnvironmentVariables()

	// An empty repo is valid: the supervisor parks until /begin provides one.
	if cfg.Repo != "" && !repoPattern.MatchString(cfg.Repo) {
		return cfg, errors.New("repo must be owner/repo")
	}
	if cfg.StateDir == "" {
		return cfg, errors.New("state_dir is required")
	}
	// Convert to absolute path if needed
	if !filepath.IsAbs(cfg.StateDir) {
		cfg.StateDir = filepath.Join(filepath.Dir(path), cfg.StateDir)
	}
	absStateDir, err := filepath.Abs(cfg.StateDir)
	if err != nil {
		return cfg, err
	}
	cfg.StateDir = absStateDir
	cfg.Path = path
	if cfg.PollSeconds <= 0 {
		cfg.PollSeconds = 60
	}
	if cfg.IdleTimeoutMinutes <= 0 {
		cfg.IdleTimeoutMinutes = 20
	}
	if cfg.StageTimeoutMinutes <= 0 {
		cfg.StageTimeoutMinutes = 180
	}
	if cfg.MaxStageRetries <= 0 {
		cfg.MaxStageRetries = 3
	}
	if cfg.MaxReviewRounds <= 0 {
		cfg.MaxReviewRounds = 3
	}
	if cfg.MaxCIFixes <= 0 {
		cfg.MaxCIFixes = 2
	}
	if cfg.MaxLocalFixes <= 0 {
		cfg.MaxLocalFixes = 3
	}
	if cfg.MaxBaseSyncRounds <= 0 {
		cfg.MaxBaseSyncRounds = 3
	}
	if cfg.CIWaitMinutes <= 0 {
		cfg.CIWaitMinutes = 30
	}
	if len(cfg.CheckCommands) == 0 && strings.TrimSpace(cfg.CheckCommand) != "" {
		cfg.CheckCommands = []string{cfg.CheckCommand}
	}
	// An empty check_commands list is valid: for a repository chosen at runtime
	// via /begin the checks come from the plan phase instead. local_verify
	// still blocks when neither source yields a check, so nothing fakes success.
	for _, check := range cfg.CheckCommands {
		if strings.TrimSpace(check) == "" {
			return cfg, errors.New("check_commands must not contain empty entries")
		}
	}
	if cfg.Models == nil {
		cfg.Models = map[string]ModelProfile{}
	}
	for _, role := range []string{"investigate", "plan", "implement", "review", "chat"} {
		profile := cfg.Models[role]
		if strings.TrimSpace(profile.ID) == "" {
			return cfg, fmt.Errorf("models.%s.id is required", role)
		}
		if profile.Provider == "" {
			// Auto-detect appropriate provider for the platform
			profile.Provider, _, _ = cfg.DetectProvider(role)
		}
		if profile.Thinking == "" {
			profile.Thinking = "off"
		}
		switch profile.Thinking {
		case "off", "minimal", "low", "medium", "high", "xhigh", "max":
		default:
			return cfg, fmt.Errorf("models.%s.thinking is invalid: %q", role, profile.Thinking)
		}
		cfg.Models[role] = profile
	}
	// triage is optional: an absent entry inherits the plan profile so that
	// existing configurations keep loading unchanged.
	if profile, ok := cfg.Models["triage"]; !ok || strings.TrimSpace(profile.ID) == "" {
		cfg.Models["triage"] = cfg.Models["plan"]
	} else {
		if profile.Provider == "" {
			profile.Provider, _, _ = cfg.DetectProvider("triage")
		}
		if profile.Thinking == "" {
			profile.Thinking = "off"
		}
		switch profile.Thinking {
		case "off", "minimal", "low", "medium", "high", "xhigh", "max":
		default:
			return cfg, fmt.Errorf("models.triage.thinking is invalid: %q", profile.Thinking)
		}
		cfg.Models["triage"] = profile
	}
	// Platform guard (issue #5): omlx without base_url is a macOS built-in.
	// On any other host it would silently target a local runtime that does not
	// exist (e.g. a Raspberry Pi quietly hitting Ollama with MLX model IDs).
	// Fail at config load instead: the fix is a base_url or another provider.
	if err := validateModelsForPlatform(cfg.Models, detectPlatform()); err != nil {
		return cfg, err
	}
	if len(cfg.AllowedModels) == 0 {
		return cfg, errors.New("allowed_models must have at least one model")
	}
	for _, role := range []string{"investigate", "plan", "implement", "review", "chat", "triage"} {
		if !cfg.ModelAllowed(cfg.Models[role].ID) {
			return cfg, fmt.Errorf("model %q is not in allowed_models", cfg.Models[role].ID)
		}
	}
	if cfg.TelegramChatID <= 0 {
		return cfg, errors.New("telegram_chat_id must be a positive authorized chat ID")
	}
	// authorized_user_id is optional: 0 means no access control (auto-discover mode allowed)
	if cfg.AuthorizedUserID < 0 {
		return cfg, errors.New("authorized_user_id must be 0 or a positive value for access control")
	}
	if cfg.ManualRepo != "" && cfg.Repo != "" {
		return cfg, errors.New("cannot set both repo and manual_repo")
	}
	if cfg.ManualMilestone != "" && cfg.Milestone != "" {
		return cfg, errors.New("cannot set both milestone and manual_milestone")
	}
	return cfg, nil
}

func (c Config) ModelAllowed(model string) bool {
	for _, allowed := range c.AllowedModels {
		if model == allowed {
			return true
		}
	}
	return false
}

// ExpandEnvironmentVariables expands ${VAR_NAME} and ${VAR_NAME:-default} patterns in the config.
// Returns a copy with all environment variables resolved.
func (c Config) ExpandEnvironmentVariables() Config {
	cfg := c
	cfg.Repo = expandEnv(cfg.Repo)
	cfg.Milestone = expandEnv(cfg.Milestone)
	cfg.Label = expandEnv(cfg.Label)
	cfg.StateDir = expandEnv(cfg.StateDir)
	cfg.PollSeconds = parseEnvInt(fmt.Sprintf("%d", cfg.PollSeconds), cfg.PollSeconds)
	cfg.IdleTimeoutMinutes = parseEnvInt(fmt.Sprintf("%d", cfg.IdleTimeoutMinutes), cfg.IdleTimeoutMinutes)
	cfg.StageTimeoutMinutes = parseEnvInt(fmt.Sprintf("%d", cfg.StageTimeoutMinutes), cfg.StageTimeoutMinutes)
	cfg.MaxStageRetries = parseEnvInt(fmt.Sprintf("%d", cfg.MaxStageRetries), cfg.MaxStageRetries)
	cfg.MaxReviewRounds = parseEnvInt(fmt.Sprintf("%d", cfg.MaxReviewRounds), cfg.MaxReviewRounds)
	cfg.MaxCIFixes = parseEnvInt(fmt.Sprintf("%d", cfg.MaxCIFixes), cfg.MaxCIFixes)
	cfg.MaxLocalFixes = parseEnvInt(fmt.Sprintf("%d", cfg.MaxLocalFixes), cfg.MaxLocalFixes)
	cfg.MaxBaseSyncRounds = parseEnvInt(fmt.Sprintf("%d", cfg.MaxBaseSyncRounds), cfg.MaxBaseSyncRounds)
	cfg.CIWaitMinutes = parseEnvInt(fmt.Sprintf("%d", cfg.CIWaitMinutes), cfg.CIWaitMinutes)
	cfg.AutoMerge = parseEnvBool(fmt.Sprintf("%t", cfg.AutoMerge), cfg.AutoMerge)
	cfg.CheckCommand = expandEnv(cfg.CheckCommand)
	cfg.ManualRepo = expandEnv(cfg.ManualRepo)
	cfg.ManualMilestone = expandEnv(cfg.ManualMilestone)

	for role, profile := range cfg.Models {
		newProfile := profile
		newProfile.Provider = expandEnv(profile.Provider)
		newProfile.ID = expandEnv(profile.ID)
		newProfile.Thinking = expandEnv(profile.Thinking)
		newProfile.BaseURL = expandEnv(profile.BaseURL)
		newProfile.APIKey = expandEnv(profile.APIKey)
		cfg.Models[role] = newProfile
	}

	return cfg
}

// expandEnv replaces ${VAR_NAME} and ${VAR_NAME:-default} patterns with environment values.
func expandEnv(s string) string {
	if s == "" {
		return s
	}

	result := s
	for {
		varName, defaultValue, found := parseEnvPattern(result)
		if !found {
			break
		}
		value := os.Getenv(varName)
		if value == "" && defaultValue != "" {
			value = defaultValue
		}
		result = strings.ReplaceAll(result, "${"+varName+"}", value)
		result = strings.ReplaceAll(result, "${"+varName+":-"+defaultValue+"}", value)
	}
	return result
}

// parseEnvPattern extracts variable name and optional default from ${...} pattern.
func parseEnvPattern(s string) (string, string, bool) {
	// Match ${VAR_NAME} or ${VAR_NAME:-default}
	if len(s) < 3 || s[0] != '$' || s[1] != '{' || s[len(s)-1] != '}' {
		return "", "", false
	}

	inner := s[2 : len(s)-1]

	// Check for default value pattern: VAR_NAME:-default
	if idx := strings.Index(inner, ":-"); idx != -1 {
		return inner[:idx], inner[idx+2:], true
	}

	// No default, just variable name
	return inner, "", true
}

// parseEnvInt parses an environment variable string as an integer.
// If os.Getenv("PI_AUTOPILOT_" + key) exists and is a valid integer, returns that value.
// Otherwise returns defaultValue.
func parseEnvInt(s string, defaultValue int) int {
	varName := "PI_AUTOPILOT_" + strings.ToUpper(strings.ReplaceAll(s, ".", "_"))
	envVal := os.Getenv(varName)
	if envVal == "" {
		return defaultValue
	}

	num, err := strconv.Atoi(strings.TrimSpace(envVal))
	if err != nil {
		return defaultValue
	}
	return num
}

// parseEnvBool parses an environment variable as boolean.
// If os.Getenv("PI_AUTOPILOT_" + key) equals "true" (case-insensitive), returns true.
// Otherwise returns defaultValue.
func parseEnvBool(s string, defaultValue bool) bool {
	varName := "PI_AUTOPILOT_" + strings.ToUpper(strings.ReplaceAll(s, ".", "_"))
	envVal := os.Getenv(varName)
	if envVal == "" {
		return defaultValue
	}

	if strings.ToLower(strings.TrimSpace(envVal)) == "true" {
		return true
	}
	return false
}

// applyManualConfig applies manual repo/milestone from config to state.
// It sets phase to waiting_for_start and marks the app to expect a /start command.
func (c Config) applyManualConfig(s *State) error {
	// Save backup of current state before clearing
	if s.Issue.Number != 0 {
		backupPath := filepath.Join(c.StateDir, "backup-state-"+strconv.Itoa(s.Issue.Number)+".json")
		data, err := os.ReadFile(filepath.Join(c.StateDir, "state.json"))
		if err == nil {
			if err := atomicBytes(backupPath, data); err != nil {
				return fmt.Errorf("backup failed: %w", err)
			}
		}
	}

	// Clear state issue and set to waiting
	s.Issue = Issue{}
	s.Phase = "waiting_for_start"
	s.Paused = false
	s.BlockedPhase = ""
	s.LastError = "Waiting for /begin <repo> [milestone] command"
	s.RecoveryPrompt = ""
	s.Attempts = map[string]int{}
	s.StageRuns = map[string]int{}
	s.CIReruns = map[string]int{}

	return nil
}

// validateModelsForPlatform rejects profiles that silently target a runtime
// absent on the running host. omlx without base_url is only meaningful on
// macOS (built-in MLX); elsewhere it must carry an explicit base_url.
// It takes the platform string (from detectPlatformFrom) so tests can
// exercise every host without depending on the build system.
func validateModelsForPlatform(models map[string]ModelProfile, platform string) error {
	if platform == "macos-arm64" || platform == "macos-amd64" {
		return nil
	}
	roles := make([]string, 0, len(models))
	for role := range models {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		p := models[role]
		if p.Provider == "omlx" && p.BaseURL == "" {
			return fmt.Errorf("models.%s: provider omlx without base_url is macOS-only; on this host set base_url to your remote OpenAI-compatible endpoint (e.g. your Mac Studio) or choose another provider", role)
		}
	}
	return nil
}

// detectPlatform returns the detected platform based on the binary's target.
func detectPlatform() string {
	return detectPlatformFrom(runtime.GOOS, runtime.GOARCH)
}

// detectPlatformFrom is a pure helper that maps a GOOS/GOARCH pair to a
// platform string. It exists so tests can exercise every branch without
// depending on the host system.
func detectPlatformFrom(goos, goarch string) string {
	lower := strings.ToLower(goos)
	if strings.Contains(lower, "darwin") {
		arch := strings.ToLower(goarch)
		if strings.Contains(arch, "arm64") || strings.Contains(arch, "aarch64") {
			return "macos-arm64"
		}
		return "macos-amd64"
	}
	if strings.Contains(lower, "linux") {
		return "linux"
	}
	return "unknown"
}

// detectProvider detects and configures the provider based on platform and custom configuration.
func (c Config) DetectProvider(role string) (string, string, string) {
	// Provider always configurable by user, regardless of platform
	provider := c.Models[role].Provider
	if provider == "" {
		// Basic auto-detection if not specified
		p := detectPlatform()
		switch p {
		case "macos-arm64", "macos-amd64":
			provider = "omlx"
		default:
			provider = "ollama"
		}
	}

	// Base URL configurable
	baseURL := c.Models[role].BaseURL
	if baseURL == "" {
		// Default URL based on provider
		switch provider {
		case "omlx":
			baseURL = ""
		case "ollama":
			baseURL = "http://localhost:11434"
		case "openai", "openai-compatible":
			baseURL = "https://api.openai.com/v1"
		case "anthropic", "anthropic-compatible":
			baseURL = "https://api.anthropic.com/v1"
		default:
			baseURL = ""
		}
	}

	// API Key
	apiKey := c.Models[role].APIKey
	if apiKey == "" {
		// Try to get from environment variables
		switch provider {
		case "openai", "openai-compatible":
			apiKey = os.Getenv("OPENAI_API_KEY")
		case "anthropic", "anthropic-compatible":
			apiKey = os.Getenv("ANTHROPIC_API_KEY")
		}
	}

	return provider, baseURL, apiKey
}

type Issue struct {
	Number    int    `json:"number"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	URL       string `json:"url"`
	State     string `json:"state"`
	Milestone struct {
		Title string `json:"title"`
	} `json:"milestone"`
}

type State struct {
	Issue Issue  `json:"issue"`
	Phase string `json:"phase"`
	// Repo and Milestone persist the target of a /begin so a supervisor
	// restarted with an empty configured repo resumes the same repository.
	Repo           string            `json:"repo,omitempty"`
	Milestone      string            `json:"milestone,omitempty"`
	Queue          []int             `json:"queue,omitempty"`
	NoWorkNotified bool              `json:"no_work_notified,omitempty"`
	BlockedPhase   string            `json:"blocked_phase,omitempty"`
	Attempts       map[string]int    `json:"attempts"`
	RoleModels     map[string]string `json:"role_models"`
	OverridesV2    bool              `json:"model_overrides_v2"`
	RecoveryPrompt string            `json:"recovery_prompt,omitempty"`
	LastError      string            `json:"last_error,omitempty"`
	PRNumber       int               `json:"pr_number,omitempty"`
	MergeRequested bool              `json:"merge_requested"`
	MergePolicy    string            `json:"merge_policy,omitempty"`
	ReviewRounds   int               `json:"review_rounds"`
	CIFixes        int               `json:"ci_fixes"`
	LocalFixes     int               `json:"local_fixes"`
	BaseSyncRounds int               `json:"base_sync_rounds"`
	// TriagedRepo names the repository the triage baseline was planned for;
	// a baseline from another repository is stale, because issue numbers are
	// per repository.
	TriagedRepo string `json:"triaged_repo,omitempty"`
	// TriagedSeen holds the open issue numbers the last full triage planned,
	// and TriagedOrder the order that covered them. The work loop re-runs the
	// LLM ordering only when the open set grows or the baseline is missing.
	TriagedSeen          []int          `json:"triaged_seen,omitempty"`
	TriagedOrder         []int          `json:"triaged_order,omitempty"`
	BaseBranch           string         `json:"base_branch,omitempty"`
	ReviewedFingerprint  string         `json:"reviewed_fingerprint,omitempty"`
	PublishedFingerprint string         `json:"published_fingerprint,omitempty"`
	PublishedSHA         string         `json:"published_sha,omitempty"`
	CIStartedAt          time.Time      `json:"ci_started_at,omitempty"`
	StageRuns            map[string]int `json:"stage_runs,omitempty"`
	CIReruns             map[string]int `json:"ci_reruns,omitempty"`
	CIRerunAt            time.Time      `json:"ci_rerun_at,omitempty"`
	Paused               bool           `json:"paused"`
	// Remote outage tracking (issue #6): persisted waiting state so a
	// supervisor restart keeps the outage episode (original start, backoff
	// schedule, notification dedup) alive across process restarts.
	// Additive fields: old state files load with zero values, no migration.
	RemoteOutageSince  time.Time `json:"remote_outage_since,omitempty"`
	RemoteHost         string    `json:"remote_host,omitempty"` // host:port only — never a key
	RemoteProbeCount   int       `json:"remote_probe_count,omitempty"`
	RemoteBackoffUntil time.Time `json:"remote_backoff_until,omitempty"`
	RemoteNotified     bool      `json:"remote_notified,omitempty"` // offline-transition dedup flag
	RemoteReminderAt   time.Time `json:"remote_reminder_at,omitempty"`
	// OOMCount is the separate bounded counter for provider OOM/context
	// failures (issue #6): distinct from Attempts, which counts code-fix retries.
	OOMCount       int       `json:"oom_count,omitempty"`
	ChatMode       bool      `json:"chat_mode"`
	ChatHistory    []string  `json:"chat_history,omitempty"`
	TelegramOffset int64     `json:"telegram_offset"`
	UpdatedAt      time.Time `json:"updated_at"`
}

func DefaultState(cfg Config) State {
	return State{Phase: "idle", Attempts: map[string]int{}, StageRuns: map[string]int{}, CIReruns: map[string]int{}, RoleModels: map[string]string{}, OverridesV2: true}
}

func LoadState(path string, cfg Config) (State, error) {
	s := DefaultState(cfg)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err = json.Unmarshal(data, &s); err != nil {
		return s, err
	}
	if s.Attempts == nil {
		s.Attempts = map[string]int{}
	}
	if s.RoleModels == nil {
		s.RoleModels = map[string]string{}
	}
	if s.StageRuns == nil {
		s.StageRuns = map[string]int{}
	}
	if s.CIReruns == nil {
		s.CIReruns = map[string]int{}
	}
	if s.Issue.Number > 0 && s.MergePolicy != "auto" && s.MergePolicy != "manual" {
		return s, errors.New("saved run lacks an explicit immutable merge_policy; refuse to infer merge authority during upgrade")
	}
	if !s.OverridesV2 {
		// v1 stored defaults as if they were overrides. Release those pinned
		// values so the YAML becomes authoritative after migration.
		s.RoleModels = map[string]string{}
		s.OverridesV2 = true
	}
	for role, model := range s.RoleModels {
		if _, exists := cfg.Models[role]; !exists {
			return s, fmt.Errorf("saved override for unknown role %q", role)
		}
		if !cfg.ModelAllowed(model) {
			return s, fmt.Errorf("saved model %q is no longer allowed", model)
		}
	}
	return s, nil
}

// atomicJSON prevents partial state files if the process is interrupted during writes.
func atomicJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return atomicBytes(path, data)
}

func atomicBytes(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".state-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err = tmp.Chmod(0600); err != nil {
		tmp.Close()
		return err
	}
	if _, err = tmp.Write(data); err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
