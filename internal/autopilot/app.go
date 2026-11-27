package autopilot

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type App struct {
	cfg          Config
	checkout     string
	statePath    string
	state        State
	mu           sync.Mutex
	telegram     *Telegram
	chatMu       sync.Mutex
	stageRunning atomic.Bool
	lockFile     *os.File
}

func NewApp(cfg Config, botToken string) (*App, error) {
	for _, tool := range []string{"git", "gh", "pi"} {
		if _, err := exec.LookPath(tool); err != nil {
			return nil, fmt.Errorf("%s not on PATH", tool)
		}
	}
	if err := os.MkdirAll(cfg.StateDir, 0700); err != nil {
		return nil, err
	}
	// The supervisor always runs Pi against the managed agent directory;
	// fail startup loudly when the registry cannot be generated, rather than
	// silently falling back to the user's global Pi configuration.
	if err := SyncManagedPi(cfg); err != nil {
		return nil, fmt.Errorf("managed Pi installation: %w", err)
	}
	lockFile, err := os.OpenFile(filepath.Join(cfg.StateDir, "supervisor.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lockFile.Close()
		return nil, fmt.Errorf("another supervisor may already be using %s: %w", cfg.StateDir, err)
	}
	statePath := filepath.Join(cfg.StateDir, "state.json")
	s, err := LoadState(statePath, cfg)
	if err != nil {
		lockFile.Close()
		return nil, err
	}

	// A config without a repository resumes the one persisted with the issue,
	// so a service restart in manual mode does not lose its target.
	if cfg.Repo == "" && s.Repo != "" {
		cfg.Repo = s.Repo
		cfg.Milestone = s.Milestone
	}

	// Apply manual repo/milestone from config if state has no active issue
	if s.Issue.Number == 0 && (cfg.Repo == "" || cfg.ManualRepo != "" || cfg.ManualMilestone != "") {
		_ = cfg.applyManualConfig(&s)
	}

	a := &App{cfg: cfg, checkout: filepath.Join(cfg.StateDir, "worktrees", fmt.Sprintf("issue-%d", s.Issue.Number)), statePath: statePath, state: s, lockFile: lockFile}
	if botToken != "" {
		a.telegram = NewTelegram(botToken, cfg.TelegramChatID)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err = a.telegram.request(ctx, "getMe", map[string]any{}, nil)
		cancel()
		if err != nil {
			a.Close()
			return nil, fmt.Errorf("Telegram authentication failed: %w", err)
		}
	}
	return a, nil
}

func (a *App) Close() error {
	if a.lockFile == nil {
		return nil
	}
	_ = syscall.Flock(int(a.lockFile.Fd()), syscall.LOCK_UN)
	return a.lockFile.Close()
}

func (a *App) snapshot() State {
	a.mu.Lock()
	defer a.mu.Unlock()
	s := a.state
	s.Attempts = make(map[string]int, len(a.state.Attempts))
	for k, v := range a.state.Attempts {
		s.Attempts[k] = v
	}
	s.StageRuns = make(map[string]int, len(a.state.StageRuns))
	for k, v := range a.state.StageRuns {
		s.StageRuns[k] = v
	}
	s.CIReruns = make(map[string]int, len(a.state.CIReruns))
	for k, v := range a.state.CIReruns {
		s.CIReruns[k] = v
	}
	s.RoleModels = make(map[string]string, len(a.state.RoleModels))
	for k, v := range a.state.RoleModels {
		s.RoleModels[k] = v
	}
	return s
}

func (a *App) update(fn func(*State)) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	fn(&a.state)
	a.state.UpdatedAt = time.Now().UTC()
	return atomicJSON(a.statePath, a.state)
}

func (a *App) notify(message string) {
	log.Println(message)
	if a.telegram == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := a.telegram.Send(ctx, message); err != nil {
			log.Printf("Telegram notification failed: %s", a.telegram.Redact(err.Error()))
		}
	}()
}

func (a *App) startManualIssue(ctx context.Context, repo, milestone string) error {
	if !repoPattern.MatchString(repo) {
		return fmt.Errorf("repo must be owner/repo")
	}
	// If there's already an active issue, save backup and clear it
	s := a.snapshot()
	if s.Issue.Number != 0 {
		if data, err := os.ReadFile(a.statePath); err == nil {
			backupPath := filepath.Join(a.cfg.StateDir, "backup-issue-"+strconv.Itoa(s.Issue.Number)+".json")
			if err := atomicBytes(backupPath, data); err != nil {
				return fmt.Errorf("cannot backup current issue state: %w", err)
			}
		}
	}
	// Apply new repo/milestone; the pipeline immediately re-triages against it.
	a.cfg.Repo = repo
	a.cfg.Milestone = milestone
	a.cfg.ManualRepo = ""
	a.cfg.ManualMilestone = ""
	backupNote := ""
	if s.Issue.Number != 0 {
		backupNote = fmt.Sprintf(" Previous issue #%d state was backed up.", s.Issue.Number)
	}
	err := a.update(func(st *State) {
		if st.Issue.Number != 0 {
			roleModels, offset := st.RoleModels, st.TelegramOffset
			*st = DefaultState(a.cfg)
			st.RoleModels, st.TelegramOffset = roleModels, offset
			st.Paused = false
		}
		st.Repo = repo
		st.Milestone = milestone
		st.Phase = "triage"
		st.Queue = nil
		st.NoWorkNotified = false
		st.LastError = ""
		// The triage baseline belongs to the previous target: issue numbers
		// are per repository, so it must be re-planned for the new one.
		st.TriagedRepo, st.TriagedSeen, st.TriagedOrder = "", nil, nil
	})
	if err != nil {
		return err
	}
	a.notify(fmt.Sprintf("Manual mode: repo=%s, milestone=%s. Ordering the open issues with label filter %q.%s", repo, milestone, a.cfg.Label, backupNote))
	return nil
}

func (a *App) resetManualConfig() error {
	// Clear manual repo/milestone
	a.cfg.ManualRepo = ""
	a.cfg.ManualMilestone = ""

	// Save config manually
	return atomicJSON(filepath.Join(a.cfg.StateDir, "config-manual-reset.json"), a.cfg)
}

func (a *App) Status() string {
	s := a.snapshot()
	if s.Issue.Number == 0 {
		if s.Paused {
			return "Autopilot paused. No active issue."
		}
		if s.Phase == "waiting_for_start" {
			return "Autopilot waiting for manual /begin <repo> [milestone] command."
		}
		if s.Phase == "no_work" {
			return "Autopilot idle: no eligible open issues for " + a.cfg.Repo + "; waiting for new work."
		}
		return "Autopilot idle; auto-discover mode; waiting for eligible GitHub issues."
	}
	status := fmt.Sprintf("Issue #%d: %s\nPhase: %s\nMerge policy: %s\nPaused: %t\nPR: #%d\nRetries: %d\nModels: investigate=%s, plan=%s, verify=%s, implement=%s, review=%s, chat=%s, triage=%s",
		s.Issue.Number, s.Issue.Title, s.Phase, s.MergePolicy, s.Paused, s.PRNumber, s.Attempts[s.Phase], a.modelFor(s, "investigate").ID,
		a.modelFor(s, "plan").ID, a.modelFor(s, "verify").ID, a.modelFor(s, "implement").ID, a.modelFor(s, "review").ID, a.chatModel(s), a.modelFor(s, "triage").ID)
	if len(s.Queue) > 0 {
		status += "\nQueue: " + queueSummary(s.Queue)
	}
	if s.LastError != "" {
		status += "\nLast error: " + truncate(s.LastError, 500)
	}
	if !s.RemoteOutageSince.IsZero() {
		status += fmt.Sprintf("\nRemote outage: %s offline since %s (probe %d; next probe by %s)",
			s.RemoteHost, s.RemoteOutageSince.Format("2006-01-02 15:04 MST"), s.RemoteProbeCount,
			s.RemoteBackoffUntil.Format("2006-01-02 15:04 MST"))
	}
	if s.OOMCount > 0 {
		status += fmt.Sprintf("\nProvider OOM/context failures: %d/%d (separate from code-fix retries)", s.OOMCount, maxOOMRetries)
	}
	return status
}

func queueSummary(queue []int) string {
	var shown []string
	for _, n := range queue {
		shown = append(shown, "#"+strconv.Itoa(n))
		if len(shown) >= 10 {
			break
		}
	}
	summary := strings.Join(shown, ", ")
	if len(queue) > len(shown) {
		summary += fmt.Sprintf(" +%d more", len(queue)-len(shown))
	}
	return summary
}

func (a *App) chatModel(s State) string {
	return a.modelFor(s, "chat").ID
}

func (a *App) modelFor(s State, role string) ModelProfile {
	profile := a.cfg.Models[role]
	if profile.ID == "" && (role == "triage" || role == "verify") {
		profile = a.cfg.Models["plan"]
	}
	if override := s.RoleModels[role]; override != "" {
		profile.ID = override
	}
	return profile
}

func (a *App) Work(ctx context.Context, once bool) error {
	if a.telegram != nil {
		go a.telegram.Poll(ctx, a)
	}
	// Show startup message only if there's a repo or milestone configured
	if a.cfg.Repo != "" {
		a.notify("Pi Autopilot started for " + a.cfg.Repo + ". Use /help for commands.")
	} else if a.cfg.ManualRepo != "" {
		a.notify("Pi Autopilot started. Waiting for /begin <repo> [milestone] command.")
	} else {
		a.notify("Pi Autopilot started. No repository configured; waiting for /begin <repo> [milestone] command.")
	}
	var initialIssue int
	for ctx.Err() == nil {
		s := a.snapshot()
		if s.Paused || s.Phase == "blocked" {
			if err := wait(ctx, 2*time.Second); err != nil {
				return err
			}
			continue
		}
		// A recovery chat can outlive /resume; never contend for local inference memory.
		if a.stageRunning.Load() {
			if err := wait(ctx, 2*time.Second); err != nil {
				return err
			}
			continue
		}
		if s.Issue.Number == 0 {
			switch s.Phase {
			case "waiting_for_start":
				if err := wait(ctx, time.Duration(a.cfg.PollSeconds)*time.Second); err != nil {
					return err
				}
				continue
			case "no_work":
				if err := wait(ctx, time.Duration(a.cfg.PollSeconds)*time.Second); err != nil {
					return err
				}
				if err := a.transition("triage"); err != nil {
					return err
				}
				continue
			}
			if s.Phase != "triage" {
				if err := a.transition("triage"); err != nil {
					return err
				}
				continue
			}
			issues, queue, err := a.nextTaskQueue(ctx, s)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				a.fail("triage", err)
				_ = wait(ctx, 10*time.Second)
				continue
			}
			head, ok := a.headIssue(issues, queue)
			if !ok {
				a.enterNoWork()
				continue
			}
			if once && initialIssue != 0 && head.Number != initialIssue {
				return nil
			}
			if initialIssue == 0 {
				initialIssue = head.Number
			}
			if err := a.beginIssue(head, queue); err != nil {
				return err
			}
			continue
		}
		if initialIssue == 0 {
			initialIssue = s.Issue.Number
		}
		a.checkout = filepath.Join(a.cfg.StateDir, "worktrees", fmt.Sprintf("issue-%d", s.Issue.Number))
		if advanced, err := a.reconcileHandoff(s); err != nil {
			return err
		} else if advanced {
			continue
		}
		if err := a.ensureCheckout(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			a.fail(s.Phase, err)
			_ = wait(ctx, 10*time.Second)
			continue
		}
		if err := a.step(ctx, s); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if err := a.handleStepFailure(ctx, s.Phase, err); err != nil {
				return err
			}
			continue
		}
		if once && a.snapshot().Issue.Number == 0 {
			return nil
		}
	}
	return ctx.Err()
}

func wait(ctx context.Context, duration time.Duration) error {
	t := time.NewTimer(duration)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// stageError carries the remote pre-flight probe result alongside a stage
// failure so ClassifyStageError can tell an unreachable endpoint (outage:
// wait and probe) from a rejected credential (execution: operator acts).
// Error/Unwrap keep the failure text and unwrapping behavior unchanged.
type stageError struct {
	err       error
	preflight *RemoteCheckResult
}

func (e *stageError) Error() string { return e.err.Error() }
func (e *stageError) Unwrap() error { return e.err }

// preflightOf extracts the probe result a stageError carried, if any.
func preflightOf(err error) *RemoteCheckResult {
	var se *stageError
	if errors.As(err, &se) {
		return se.preflight
	}
	return nil
}

// handleStepFailure classifies a step failure and routes it to the matching
// recovery path (issue #6):
//   - remote-outage: persist the offline/waiting state, wait and probe with
//     bounded backoff until the endpoint returns; never touch Attempts.
//   - oom: separate bounded counter with its own backoff and block message;
//     never touch Attempts.
//   - execution: the existing fail() code-fix retry→block behavior, unchanged.
//
// A transient connection failure on a stage whose profile has no remote
// endpoint (local provider hiccup, or a deterministic stage) keeps today's
// plain retry: there is no remote endpoint to probe.
func (a *App) handleStepFailure(ctx context.Context, phase string, err error) error {
	preflight := preflightOf(err)
	class := ClassifyStageError(err, preflight)
	switch class {
	case FailureRemoteOutage:
		s := a.snapshot()
		if preflight == nil && !probeableProfile(a.modelFor(s, phase)) {
			a.fail(phase, err)
			_ = wait(ctx, stageRetryDelay)
			return nil
		}
		return a.waitOutage(ctx, phase, err)
	case FailureOOM:
		if delay := a.handleOOMFailure(phase, err); delay > 0 {
			_ = wait(ctx, delay)
		}
		return nil
	default:
		a.fail(phase, err)
		_ = wait(ctx, stageRetryDelay)
		return nil
	}
}

// handleOOMFailure records one provider OOM/context failure in the separate
// OOMCount and returns the retry delay (0 once the phase is blocked). The
// block message is distinct from the code-fix retry block: the operator
// should lower provider load or pick a smaller model, then /retry.
func (a *App) handleOOMFailure(phase string, err error) time.Duration {
	_ = a.update(func(s *State) {
		s.OOMCount++
		s.LastError = truncate(err.Error(), 1800)
		if s.OOMCount >= maxOOMRetries {
			s.Phase, s.BlockedPhase = "blocked", phase
		}
	})
	s := a.snapshot()
	if s.Phase == "blocked" {
		a.notify(fmt.Sprintf("⛔ Blocked on issue #%d, stage %s after %d provider memory/context failures (separate bounded counter; code-fix retries untouched).\n%s\n\nLower the provider load or pick a smaller model, then send /retry.",
			s.Issue.Number, phase, s.OOMCount, truncate(err.Error(), 900)))
		return 0
	}
	delay := oomBackoff(s.OOMCount)
	a.notify(fmt.Sprintf("⚠️ Issue #%d: %s hit a provider memory/context limit (%d/%d); retrying in %s without consuming code-fix retries.\n%s",
		s.Issue.Number, phase, s.OOMCount, maxOOMRetries, delay, truncate(err.Error(), 450)))
	return delay
}

// waitOutage persists the offline/waiting state and probes the remote
// endpoints with bounded exponential backoff until they report healthy
// again (AC-2/AC-4). Telegram is notified exactly once on the offline
// transition, once on the online transition, and at most once per hour as a
// "still offline" reminder (AC-5). Stage retry counters are never touched
// (AC-3); on recovery this returns and the Work loop re-runs the step,
// which launches a fresh Pi session from the immutable handoff artifacts.
// Persisted fields survive a supervisor restart: the episode start and the
// backoff deadline are kept, never reset.
func (a *App) waitOutage(ctx context.Context, phase string, err error) error {
	host := a.outageHost(phase)
	_ = a.update(func(s *State) {
		s.LastError = truncate(err.Error(), 1800)
		if s.RemoteHost == "" {
			s.RemoteHost = host
		}
	})
	for ctx.Err() == nil {
		s := a.snapshot()
		now := time.Now().UTC()
		// Respect the persisted backoff deadline (after a restart it may
		// already be set): sleep to it, never re-probe hot.
		if !s.RemoteBackoffUntil.IsZero() && s.RemoteBackoffUntil.After(now) {
			if err := wait(ctx, s.RemoteBackoffUntil.Sub(now)); err != nil {
				return err
			}
		}
		now = time.Now().UTC()
		healthy := a.remoteEndpointsHealthy(ctx)
		if host == "" {
			host = s.RemoteHost
		}
		d := decideOutage(host, phase, s, healthy, s.RemoteProbeCount+1, now)
		_ = a.update(func(st *State) {
			if d.ResetOutage {
				st.RemoteOutageSince = time.Time{}
				st.RemoteHost = ""
				st.RemoteProbeCount = 0
				st.RemoteBackoffUntil = time.Time{}
				st.RemoteNotified = false
				st.RemoteReminderAt = time.Time{}
			} else {
				st.RemoteOutageSince = d.OutageSince
				st.RemoteProbeCount = d.ProbeCount
				st.RemoteBackoffUntil = d.BackoffUntil
				st.RemoteNotified = d.Notified
				st.RemoteReminderAt = d.ReminderAt
			}
		})
		switch {
		case d.NotifyOffline:
			a.notify(d.OfflineMessage)
		case d.NotifyOnline:
			a.notify(d.OnlineMessage)
		}
		if d.NotifyReminder {
			a.notify(d.ReminderMessage)
		}
		if d.ResetOutage {
			return nil
		}
	}
	return ctx.Err()
}

// remoteEndpointsHealthy probes every probeable endpoint in the config and
// reports whether the recovery probe passed: no unreachable or auth-failed
// result. A catalog miss (model-not-found) is tolerated, matching the stage
// pre-flight semantics (only connect/auth errors hard-fail).
func (a *App) remoteEndpointsHealthy(ctx context.Context) bool {
	for _, res := range CheckRemoteEndpoints(ctx, nil, a.cfg) {
		if res.Status == StatusUnreachable || res.Status == StatusAuthFailed {
			return false
		}
	}
	return true
}

// outageHost resolves the host:port shown in outage messages. State wins so
// a restart keeps the same host; otherwise the failing stage's model profile
// supplies it. host:port only — never a key value (repo invariant).
func (a *App) outageHost(phase string) string {
	s := a.snapshot()
	if s.RemoteHost != "" {
		return s.RemoteHost
	}
	if profile := a.modelFor(s, phase); profile.BaseURL != "" && profile.Provider != "ollama" {
		return hostPortOf(profile.BaseURL)
	}
	return "the remote inference endpoint"
}

func (a *App) transition(phase string) error {
	return a.update(func(s *State) {
		s.Attempts[s.Phase] = 0
		s.Phase, s.LastError, s.RecoveryPrompt = phase, "", ""
		// A stage boundary ends any remote-outage episode the previous stage
		// persisted (e.g. the supervisor crashed while waiting and the
		// endpoint recovered before the restart): a stale episode must not
		// bleed into the next stage's outage notifications or backoff.
		s.RemoteOutageSince = time.Time{}
		s.RemoteHost = ""
		s.RemoteProbeCount = 0
		s.RemoteBackoffUntil = time.Time{}
		s.RemoteNotified = false
		s.RemoteReminderAt = time.Time{}
	})
}

// beginIssue resets every per-issue counter and moves the supervisor to the
// first delivery stage. The triage queue is persisted so a restarted service
// knows what follows the current task.
func (a *App) beginIssue(issue Issue, queue []int) error {
	err := a.update(func(s *State) {
		s.Issue = issue
		s.Phase = "repository_setup"
		s.Repo = a.cfg.Repo
		s.Milestone = a.cfg.Milestone
		s.Queue = queue
		s.NoWorkNotified = false
		s.Attempts = map[string]int{}
		s.StageRuns = map[string]int{}
		s.CIReruns = map[string]int{}
		s.ReviewRounds, s.CIFixes, s.LocalFixes, s.BaseSyncRounds, s.PRNumber = 0, 0, 0, 0, 0
		s.BaseBranch, s.ReviewedFingerprint, s.PublishedFingerprint, s.PublishedSHA = "", "", "", ""
		s.CIStartedAt = time.Time{}
		s.CIRerunAt = time.Time{}
		// Per-issue remote-outage episode and OOM counter reset with every
		// issue (issue #6): a new task starts with a clean outage state.
		s.RemoteOutageSince = time.Time{}
		s.RemoteHost = ""
		s.RemoteProbeCount = 0
		s.RemoteBackoffUntil = time.Time{}
		s.RemoteNotified = false
		s.RemoteReminderAt = time.Time{}
		s.OOMCount = 0
		s.MergePolicy = "manual"
		if a.cfg.AutoMerge {
			s.MergePolicy = "auto"
		}
		s.RecoveryPrompt, s.LastError, s.BlockedPhase = "", "", ""
	})
	if err != nil {
		return err
	}
	a.checkout = filepath.Join(a.cfg.StateDir, "worktrees", fmt.Sprintf("issue-%d", issue.Number))
	a.notify(fmt.Sprintf("Started issue #%d: %s\n%s\nQueue: %s", issue.Number, issue.Title, issue.URL, queueSummary(queue)))
	return nil
}

// enterNoWork parks the supervisor while the repository has no eligible open
// issues. The operator is told exactly once per empty episode; subsequent
// empty polls only log and wait.
func (a *App) enterNoWork() {
	s := a.snapshot()
	if !s.NoWorkNotified {
		filter := ""
		if a.cfg.Milestone != "" {
			filter += " milestone=" + a.cfg.Milestone
		}
		if a.cfg.Label != "" {
			filter += " label=" + a.cfg.Label
		}
		a.notify(fmt.Sprintf("🛌 %s%s: no eligible open issues (skipped and completed ones are excluded). I have no work right now and will keep polling every %ds; I start automatically when eligible issues appear. Send /begin <repo> [milestone] to switch target.",
			a.cfg.Repo, filter, a.cfg.PollSeconds))
	}
	_ = a.update(func(st *State) {
		st.Repo = a.cfg.Repo
		st.Milestone = a.cfg.Milestone
		st.Queue = nil
		st.Phase = "no_work"
		st.NoWorkNotified = true
		st.LastError = ""
	})
}

func (a *App) fail(phase string, err error) {
	_ = a.update(func(s *State) {
		s.Attempts[phase]++
		s.LastError = truncate(err.Error(), 1800)
		if s.Attempts[phase] >= a.cfg.MaxStageRetries {
			s.Phase, s.BlockedPhase = "blocked", phase
		}
	})
	s := a.snapshot()
	if s.Phase == "blocked" {
		a.notify(fmt.Sprintf("⛔ Blocked on issue #%d, stage %s after %d attempts.\nError: %s\n\nSend /models, /use <role> <model>, /chat or /fix <prompt> to recover.",
			s.Issue.Number, phase, s.Attempts[phase], truncate(err.Error(), 900)))
		return
	}
	retryKind := "Retrying the deterministic operation, preserving all evidence."
	if phase == "investigate" || phase == "plan" || phase == "implement" || phase == "review" {
		retryKind = "Relaunching Pi in a completely new session, preserving artifacts and worktree."
	}
	a.notify(fmt.Sprintf("⚠️ Issue #%d: %s failed (%d/%d). %s\n%s",
		s.Issue.Number, phase, s.Attempts[phase], a.cfg.MaxStageRetries, retryKind, truncate(err.Error(), 450)))
}

func (a *App) artifact(issueNumber int, name string) string {
	return filepath.Join(a.cfg.StateDir, "artifacts", strconv.Itoa(issueNumber), name)
}

func (a *App) storeArtifact(issueNumber int, name, text string) error {
	path := a.artifact(issueNumber, name)
	return atomicBytes(path, []byte(text))
}

func (a *App) readArtifact(issueNumber int, name string) string {
	data, _ := os.ReadFile(a.artifact(issueNumber, name))
	return string(data)
}

func (a *App) runStage(ctx context.Context, s State, role, prompt string, readOnly bool) (string, error) {
	res, err := a.runStageIn(ctx, s, role, prompt, readOnly, a.checkout)
	if err != nil {
		return "", err
	}
	return res.Text, nil
}

// runStageIn runs one Pi session for a role and returns the full result:
// Text is the final assistant message, FullText the whole session record.
func (a *App) runStageIn(ctx context.Context, s State, role, prompt string, readOnly bool, dir string) (PiResult, error) {
	a.stageRunning.Store(true)
	defer a.stageRunning.Store(false)
	profile := a.modelFor(s, role)
	// Remote no-fallback pre-flight (issue #5): when the role's profile points
	// at a user-declared remote endpoint, verify it is reachable and accepts
	// the credential *before* launching Pi. A down remote endpoint must fail
	// this stage with an actionable error, never silently substitute a local
	// provider (Ollama) for the model the plan was written for.
	if err, preflight := a.remotePreflight(ctx, role, profile); err != nil {
		return PiResult{Err: err}, &stageError{err: err, preflight: preflight}
	}
	session := time.Now().UTC().Format("20060102T150405.000000000")
	sessionsDir := filepath.Join(a.cfg.StateDir, "sessions")
	if s.Issue.Number > 0 {
		sessionsDir = a.artifact(s.Issue.Number, "sessions")
	}
	path := filepath.Join(sessionsDir, fmt.Sprintf("%s-%s.jsonl", role, session))
	if s.RecoveryPrompt != "" && role != "chat" {
		prompt += "\n\nAdditional operator instruction after a previous failure:\n" + s.RecoveryPrompt
	}
	handoff := a.handoffContext(s)
	if handoff == "" && role == "implement" {
		// Supports an in-flight v1 issue that predates handoff manifests.
		handoff = "Existing plan (plan.md):\n" + truncate(a.readArtifact(s.Issue.Number, "plan.md"), 12000) + "\n"
	}
	prompt = handoff + prompt
	res := RunPi(ctx, a.cfg, dir, role, PiProviderKey(profile), profile.ID, profile.Thinking, prompt, path, readOnly)
	if res.Err != nil {
		return res, fmt.Errorf("%s (log: %s)", res.Err, res.LogPath)
	}
	return res, nil
}

// remotePreflight probes the role's remote endpoint before a Pi session is
// launched. Only profiles with an explicit non-default base_url (a user-
// declared remote endpoint) are checked; local providers and provider
// defaults are skipped so the pre-flight adds latency only where the
// Raspberry→Mac Studio (or similar) topology actually exists. It hard-fails
// on connect and auth errors only; a catalog miss is left to Pi/doctor.
// The probe result is returned (nil when no remote endpoint was probed) so
// the stage failure can be classified: unreachable -> remote-outage (wait
// and probe), auth-failed -> execution (credential does not self-heal).
func (a *App) remotePreflight(ctx context.Context, role string, profile ModelProfile) (error, *RemoteCheckResult) {
	if profile.BaseURL == "" || profile.Provider == "ollama" {
		return nil, nil
	}
	var cfg Config
	cfg.Models = map[string]ModelProfile{"preflight": profile}
	for _, res := range CheckRemoteEndpoints(ctx, nil, cfg) {
		if res.Status == StatusUnreachable || res.Status == StatusAuthFailed {
			return fmt.Errorf("%s stage: remote endpoint %s: %s — not falling back to a local provider; fix the endpoint or its credential", role, hostPortOf(profile.BaseURL), res.Message), &res
		}
	}
	return nil, nil
}
