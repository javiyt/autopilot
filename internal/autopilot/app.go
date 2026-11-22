package autopilot

import (
	"context"
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
	
	// Apply manual repo/milestone from config if state has no active issue
	if s.Issue.Number == 0 && (cfg.ManualRepo != "" || cfg.ManualMilestone != "") {
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
	s := a.snapshot()
	
	// If there's already an active issue, save backup and clear it
	if s.Issue.Number != 0 {
		backupPath := filepath.Join(a.cfg.StateDir, "backup-issue-"+strconv.Itoa(s.Issue.Number)+".json")
		data, err := os.ReadFile(a.statePath)
		if err == nil {
			if err := atomicBytes(backupPath, data); err != nil {
				return fmt.Errorf("cannot backup current issue: %w", err)
			}
		}
		// Clear the current issue state
		s.Issue = Issue{}
		s.Phase = "waiting_for_start"
		s.Attempts = map[string]int{}
		s.StageRuns = map[string]int{}
		s.CIReruns = map[string]int{}
		s.ReviewRounds, s.CIFixes, s.LocalFixes, s.PRNumber = 0, 0, 0, 0
		s.BaseBranch, s.ReviewedFingerprint, s.PublishedFingerprint, s.PublishedSHA = "", "", "", ""
		s.CIStartedAt, s.CIRerunAt = time.Time{}, time.Time{}
		s.MergePolicy = "manual"
		s.RecoveryPrompt, s.LastError, s.BlockedPhase = "", "", ""
		s.Paused = false
	}
	
	// Apply new repo/milestone
	a.cfg.Repo = repo
	a.cfg.Milestone = milestone
	
	// Start processing issues with new config
	a.notify(fmt.Sprintf("Manual mode: repo=%s, milestone=%s. Looking for issues with label=%q", repo, milestone, a.cfg.Label))
	
	// Reload state
	a.state = s
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
		return "Autopilot idle; auto-discover mode; waiting for eligible GitHub issues."
	}
	status := fmt.Sprintf("Issue #%d: %s\nPhase: %s\nMerge policy: %s\nPaused: %t\nPR: #%d\nRetries: %d\nModels: investigate=%s, plan=%s, implement=%s, review=%s, chat=%s",
		s.Issue.Number, s.Issue.Title, s.Phase, s.MergePolicy, s.Paused, s.PRNumber, s.Attempts[s.Phase], a.modelFor(s, "investigate").ID,
		a.modelFor(s, "plan").ID, a.modelFor(s, "implement").ID, a.modelFor(s, "review").ID, a.chatModel(s))
	if s.LastError != "" {
		status += "\nLast error: " + truncate(s.LastError, 500)
	}
	return status
}

func (a *App) chatModel(s State) string {
	return a.modelFor(s, "chat").ID
}

func (a *App) modelFor(s State, role string) ModelProfile {
	profile := a.cfg.Models[role]
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
	if a.cfg.Repo != "" || a.cfg.Milestone != "" || a.cfg.ManualRepo != "" || a.cfg.ManualMilestone != "" {
		if a.cfg.Repo != "" {
			a.notify("Pi Autopilot started for " + a.cfg.Repo + ". Use /help for commands.")
		} else if a.cfg.ManualRepo != "" {
			a.notify("Pi Autopilot started. Waiting for /begin <repo> [milestone] command.")
		} else {
			a.notify("Pi Autopilot started. Auto-discover mode. Use /help for commands.")
		}
	} else {
		a.notify("Pi Autopilot started. Auto-discover mode. Use /help for commands.")
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
			// Check if waiting for manual start
			if s.Phase == "waiting_for_start" {
				if err := wait(ctx, time.Duration(a.cfg.PollSeconds)*time.Second); err != nil {
					return err
				}
				continue
			}
			
			// Auto-discover mode (no issue, not waiting)
			issue, err := a.nextIssue(ctx)
			if err != nil {
				a.notify("Unable to list issues: " + truncate(err.Error(), 400))
				if err = wait(ctx, time.Duration(a.cfg.PollSeconds)*time.Second); err != nil {
					return err
				}
				continue
			}
			if issue == nil {
				if err = wait(ctx, time.Duration(a.cfg.PollSeconds)*time.Second); err != nil {
					return err
				}
				continue
			}
			if once && initialIssue != 0 && issue.Number != initialIssue {
				return nil
			}
			if initialIssue == 0 {
				initialIssue = issue.Number
			}
			if err = a.update(func(s *State) {
				s.Issue = *issue
				s.Phase = "repository_setup"
				s.Attempts = map[string]int{}
				s.StageRuns = map[string]int{}
				s.CIReruns = map[string]int{}
				s.ReviewRounds, s.CIFixes, s.LocalFixes, s.PRNumber = 0, 0, 0, 0
				s.BaseBranch, s.ReviewedFingerprint, s.PublishedFingerprint, s.PublishedSHA = "", "", "", ""
				s.CIStartedAt = time.Time{}
				s.CIRerunAt = time.Time{}
				s.MergePolicy = "manual"
				if a.cfg.AutoMerge {
					s.MergePolicy = "auto"
				}
				s.RecoveryPrompt, s.LastError, s.BlockedPhase = "", "", ""
			}); err != nil {
				return err
			}
			a.checkout = filepath.Join(a.cfg.StateDir, "worktrees", fmt.Sprintf("issue-%d", issue.Number))
			a.notify(fmt.Sprintf("Started issue #%d: %s\n%s", issue.Number, issue.Title, issue.URL))
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
			a.fail(s.Phase, err)
			delay := 5 * time.Second
			if strings.Contains(err.Error(), "memory/context") || strings.Contains(err.Error(), "possible OOM") {
				delay = time.Duration(20*a.snapshot().Attempts[s.Phase]) * time.Second
			}
			_ = wait(ctx, delay)
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

func (a *App) transition(phase string) error {
	return a.update(func(s *State) {
		s.Attempts[s.Phase] = 0
		s.Phase, s.LastError, s.RecoveryPrompt = phase, "", ""
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
	a.stageRunning.Store(true)
	defer a.stageRunning.Store(false)
	profile := a.modelFor(s, role)
	session := time.Now().UTC().Format("20060102T150405.000000000")
	path := a.artifact(s.Issue.Number, fmt.Sprintf("sessions/%s-%s.jsonl", role, session))
	if s.RecoveryPrompt != "" && role != "chat" {
		prompt += "\n\nAdditional operator instruction after a previous failure:\n" + s.RecoveryPrompt
	}
	handoff := a.handoffContext(s)
	if handoff == "" && role == "implement" {
		// Supports an in-flight v1 issue that predates handoff manifests.
		handoff = "Existing plan (plan.md):\n" + truncate(a.readArtifact(s.Issue.Number, "plan.md"), 12000) + "\n"
	}
	prompt = handoff + prompt
	res := RunPi(ctx, a.cfg, a.checkout, role, profile.Provider, profile.ID, profile.Thinking, prompt, path, readOnly)
	if res.Err != nil {
		return "", fmt.Errorf("%s (log: %s)", res.Err, res.LogPath)
	}
	return res.Text, nil
}
