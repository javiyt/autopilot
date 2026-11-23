package autopilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Handoff is the durable contract between independent Pi sessions. A phase
// writes its report and this manifest before moving the supervisor's state.
type Handoff struct {
	IssueNumber int       `json:"issue_number"`
	MergePolicy string    `json:"merge_policy"`
	FromPhase   string    `json:"from_phase"`
	ToPhase     string    `json:"to_phase"`
	Model       string    `json:"model,omitempty"`
	ReportFile  string    `json:"report_file"`
	StatusFile  string    `json:"status_file"`
	CompletedAt time.Time `json:"completed_at"`
}

func (a *App) latestHandoff(issueNumber int) (Handoff, error) {
	var h Handoff
	data, err := os.ReadFile(a.artifact(issueNumber, "handoff.json"))
	if err != nil {
		return h, err
	}
	if err := json.Unmarshal(data, &h); err != nil {
		return h, err
	}
	if h.IssueNumber != issueNumber {
		return h, fmt.Errorf("handoff issue mismatch: %d", h.IssueNumber)
	}
	return h, nil
}

func (a *App) recordHandoff(ctx context.Context, s State, from, next, report string) error {
	if s.Issue.Number == 0 {
		return fmt.Errorf("cannot hand off without an issue")
	}
	if s.MergePolicy != "auto" && s.MergePolicy != "manual" {
		return fmt.Errorf("missing immutable merge policy")
	}
	if strings.TrimSpace(report) == "" {
		return fmt.Errorf("cannot hand off an empty report")
	}
	// Each successful attempt is immutable. The short canonical copy is just a
	// latest-view convenience; handoffs point to the attempt, never to its alias.
	timestamp := time.Now().UTC().Format("20060102T150405.000000000")
	reportName := filepath.Join("attempts", from, timestamp+".md")
	if err := a.storeArtifact(s.Issue.Number, reportName, report); err != nil {
		return err
	}
	if err := a.storeArtifact(s.Issue.Number, canonicalReport(from), report); err != nil {
		return err
	}
	gitStatus, err := command(ctx, a.checkout, "git", "status", "--short")
	if err != nil {
		return err
	}
	statusName := filepath.Join("attempts", from, timestamp+".status.txt")
	if err := a.storeArtifact(s.Issue.Number, statusName, gitStatus); err != nil {
		return err
	}
	model := ""
	if _, ok := a.cfg.Models[from]; ok {
		model = a.modelFor(s, from).ID
	}
	handoff := Handoff{
		IssueNumber: s.Issue.Number, MergePolicy: s.MergePolicy, FromPhase: from, ToPhase: next, Model: model,
		ReportFile: reportName, StatusFile: statusName, CompletedAt: time.Now().UTC(),
	}
	// Historical handoffs help diagnose past corrections; the latest handoff
	// is read by the next session. The state transition happens only after it.
	if err := atomicJSON(a.artifact(s.Issue.Number, filepath.Join("attempts", from, timestamp+".handoff.json")), handoff); err != nil {
		return err
	}
	return atomicJSON(a.artifact(s.Issue.Number, "handoff.json"), handoff)
}

func (a *App) handoffContext(s State) string {
	h, err := a.latestHandoff(s.Issue.Number)
	if err != nil || h.ToPhase != s.Phase {
		return ""
	}
	// Only read known files inside the issue artifact directory, never a path
	// provided by a model or Telegram message.
	if !safeArtifactRelative(h.ReportFile) || !safeArtifactRelative(h.StatusFile) {
		return ""
	}
	report := a.readArtifact(s.Issue.Number, h.ReportFile)
	status := a.readArtifact(s.Issue.Number, h.StatusFile)
	if strings.TrimSpace(report) == "" {
		return ""
	}
	context := fmt.Sprintf("ARTIFACT-ONLY HANDOFF (NOT A PREVIOUS CONVERSATION)\nIssue #%d; completed phase: %s; next phase: %s; model: %s; merge policy: %s\nImmutable artifact paths:\n%s\n%s\nPrior stage evidence (%s):\n%s\nSaved git status:\n%s\n",
		h.IssueNumber, h.FromPhase, h.ToPhase, h.Model, h.MergePolicy,
		a.artifact(s.Issue.Number, h.ReportFile), a.artifact(s.Issue.Number, h.StatusFile),
		h.ReportFile, truncate(report, 12000), truncate(status, 4000))
	if s.Phase == "review" {
		context += "Implementation report (" + a.artifact(s.Issue.Number, "implementation.md") + "):\n" + truncate(a.readArtifact(s.Issue.Number, "implementation.md"), 8000) + "\n"
	}
	if s.Phase == "implement" && h.FromPhase != "plan" {
		context += "Original plan (plan.md):\n" + truncate(a.readArtifact(s.Issue.Number, "plan.md"), 8000) + "\n"
	}
	for _, stage := range deliveryStages {
		file := a.artifact(s.Issue.Number, canonicalReport(stage))
		if _, err := os.Stat(file); err == nil {
			context += "Earlier factual artifact: " + file + "\n"
		}
	}
	context += "Raw GitHub issue: " + a.artifact(s.Issue.Number, "issue.json") + "\n"
	return context + "END HANDOFF\n\n"
}

func safeArtifactRelative(name string) bool {
	clean := filepath.Clean(name)
	return name != "" && clean == name && !filepath.IsAbs(name) &&
		clean != ".." && !strings.HasPrefix(clean, ".."+string(filepath.Separator))
}

// If the supervisor died after persisting a handoff but before updating
// state.json, complete that transition without rerunning the prior phase.
func (a *App) reconcileHandoff(s State) (bool, error) {
	h, err := a.latestHandoff(s.Issue.Number)
	if err != nil || h.FromPhase != s.Phase || h.ToPhase == "" || h.ToPhase == s.Phase {
		return false, nil
	}
	if strings.TrimSpace(a.readArtifact(s.Issue.Number, h.ReportFile)) == "" {
		return false, nil
	}
	if h.MergePolicy != s.MergePolicy {
		return false, errors.New("handoff merge policy differs from immutable run policy")
	}
	if h.FromPhase == "report" && h.ToPhase == "done" {
		if err := a.complete(s.Issue); err != nil {
			return false, err
		}
		return true, nil
	}
	if err := a.transition(h.ToPhase); err != nil {
		return false, err
	}
	return true, nil
}
