package autopilot

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func (a *App) gh(ctx context.Context, args ...string) (string, error) {
	args = append(args, "--repo", a.cfg.Repo)
	return command(ctx, "", "gh", args...)
}

func (a *App) nextIssue(ctx context.Context) (*Issue, error) {
	args := []string{"issue", "list", "--state", "open", "--limit", "1000", "--json", "number,title,body,url,state,milestone"}
	if a.cfg.Milestone != "" {
		args = append(args, "--milestone", a.cfg.Milestone)
	}
	if a.cfg.Label != "" {
		args = append(args, "--label", a.cfg.Label)
	}
	out, err := a.gh(ctx, args...)
	if err != nil {
		return nil, err
	}
	var issues []Issue
	if err = json.Unmarshal([]byte(out), &issues); err != nil {
		return nil, err
	}
	sort.Slice(issues, func(i, j int) bool { return issues[i].Number < issues[j].Number })
	for _, issue := range issues {
		// Ignore issues explicitly excluded by the operator; a "blocked" issue
		// must be handled via Telegram, never silently replaced by a later one.
		if a.isSkipped(issue.Number) || a.isCompleted(issue.Number) {
			continue
		}
		return &issue, nil
	}
	return nil, nil
}

func (a *App) isSkipped(number int) bool {
	return a.hasRecord("skipped.json", number)
}

func (a *App) isCompleted(number int) bool {
	return a.hasRecord("completed.json", number)
}

func (a *App) hasRecord(filename string, number int) bool {
	data, err := os.ReadFile(filepath.Join(a.cfg.StateDir, filename))
	if err != nil {
		return false
	}
	var skipped map[int]bool
	return json.Unmarshal(data, &skipped) == nil && skipped[number]
}

func (a *App) skipIssue(number int) error {
	return a.recordIssue("skipped.json", number)
}

func (a *App) recordIssue(filename string, number int) error {
	path := filepath.Join(a.cfg.StateDir, filename)
	var skipped map[int]bool
	data, _ := os.ReadFile(path)
	_ = json.Unmarshal(data, &skipped)
	if skipped == nil {
		skipped = make(map[int]bool)
	}
	skipped[number] = true
	return atomicJSON(path, skipped)
}

// ensureCheckout creates a dedicated clone once, without selecting an issue
// branch or discarding pre-existing local work. Repository setup owns that gate.
func (a *App) ensureCheckout(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(a.checkout, ".git")); errors.Is(err, os.ErrNotExist) {
		if err = os.MkdirAll(filepath.Dir(a.checkout), 0700); err != nil {
			return err
		}
		if _, err = command(ctx, "", "gh", "repo", "clone", a.cfg.Repo, a.checkout, "--", "--quiet"); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	return nil
}

// prepareIssueBranch is called at implementation, never during investigation
// or planning. A rework round reuses the same branch and its uncommitted files.
func (a *App) prepareIssueBranch(ctx context.Context, issue Issue) error {
	branch := branchFor(issue.Number)
	if _, err := os.Stat(filepath.Join(a.checkout, ".git")); err != nil {
		return err
	}
	current, err := command(ctx, a.checkout, "git", "branch", "--show-current")
	if err != nil {
		return err
	}
	if strings.TrimSpace(current) == branch {
		return nil
	}
	status, err := command(ctx, a.checkout, "git", "status", "--porcelain")
	if err != nil {
		return err
	}
	if strings.TrimSpace(status) != "" {
		return errors.New("checkout contains uncommitted changes; refusing to switch branches")
	}
	if _, err = command(ctx, a.checkout, "git", "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		_, err = command(ctx, a.checkout, "git", "switch", branch)
		return err
	}
	if _, err = command(ctx, a.checkout, "git", "show-ref", "--verify", "--quiet", "refs/remotes/origin/"+branch); err == nil {
		_, err = command(ctx, a.checkout, "git", "switch", "-c", branch, "--track", "origin/"+branch)
		return err
	}
	base := a.snapshot().BaseBranch
	if base == "" {
		return errors.New("default branch not recorded by repository_setup")
	}
	_, err = command(ctx, a.checkout, "git", "switch", "-c", branch, "origin/"+base)
	return err
}

func branchFor(number int) string { return "autopilot/issue-" + strconv.Itoa(number) }

func (a *App) publish(ctx context.Context, issue Issue) (int, error) {
	branch := branchFor(issue.Number)
	existingPR, err := a.prForBranch(ctx, branch)
	if err != nil {
		return 0, err
	}
	current, err := command(ctx, a.checkout, "git", "branch", "--show-current")
	if err != nil || strings.TrimSpace(current) != branch {
		return 0, errors.New("not on the expected issue branch")
	}
	fingerprint, err := a.diffFingerprint(ctx)
	if err != nil {
		return 0, err
	}
	if fingerprint != a.snapshot().ReviewedFingerprint || fingerprint == "" {
		return 0, errors.New("source differs from independently reviewed content; return to local verification and review")
	}
	status, err := command(ctx, a.checkout, "git", "status", "--porcelain")
	if err != nil {
		return 0, err
	}
	if strings.TrimSpace(status) != "" {
		// All source files in this dedicated clean clone belong to this issue.
		// External evidence is deliberately kept outside the repository.
		if _, err = command(ctx, a.checkout, "git", "add", "--all"); err != nil {
			return 0, err
		}
		if _, err = command(ctx, a.checkout, "git", "commit", "-m", fmt.Sprintf("Implement #%d: %s", issue.Number, truncate(issue.Title, 60))); err != nil {
			return 0, err
		}
	}
	// An empty issue is not a successful implementation.
	base := a.snapshot().BaseBranch
	if base == "" {
		return 0, errors.New("base branch was not established")
	}
	changes, err := command(ctx, a.checkout, "git", "rev-list", "--count", "origin/"+base+"..HEAD")
	if err != nil || strings.TrimSpace(changes) == "0" {
		return 0, errors.New("no commits found for issue; PR creation refused")
	}
	if _, err = command(ctx, a.checkout, "git", "push", "--set-upstream", "origin", branch); err != nil {
		return 0, err
	}
	after, err := a.diffFingerprint(ctx)
	if err != nil || after != fingerprint {
		return 0, errors.New("content changed during publication")
	}
	if existingPR > 0 {
		// Reuse the PR, but refresh its description to keep the closing keyword
		// and the local evidence correct after an additional repair commit.
		body := a.prBody(issue)
		if _, err := a.gh(ctx, "pr", "edit", strconv.Itoa(existingPR), "--body", body); err != nil {
			return 0, err
		}
		return existingPR, nil
	}
	prBody := a.prBody(issue)
	_, err = a.gh(ctx, "pr", "create", "--base", base, "--head", branch, "--title", issue.Title, "--body", prBody)
	if err != nil {
		// The network can fail after GitHub created the PR; reconcile before retrying.
		if existing, _ := a.prForBranch(ctx, branch); existing > 0 {
			return existing, nil
		}
		return 0, err
	}
	return a.prForBranch(ctx, branch)
}

func (a *App) prForBranch(ctx context.Context, branch string) (int, error) {
	out, err := a.gh(ctx, "pr", "list", "--state", "all", "--head", branch, "--json", "number,state", "--limit", "20")
	if err != nil {
		return 0, err
	}
	var prs []struct {
		Number int    `json:"number"`
		State  string `json:"state"`
	}
	if err = json.Unmarshal([]byte(out), &prs); err != nil {
		return 0, err
	}
	for _, pr := range prs {
		if pr.State == "OPEN" || pr.State == "MERGED" {
			return pr.Number, nil
		}
	}
	return 0, nil
}

type CheckSummary struct {
	Pending     bool
	Failed      bool
	Success     bool
	Details     string
	FailedLinks []string
}

func ParseChecks(data []byte) (CheckSummary, error) {
	var checks []struct {
		Name   string `json:"name"`
		Bucket string `json:"bucket"`
		State  string `json:"state"`
		Link   string `json:"link"`
	}
	if err := json.Unmarshal(data, &checks); err != nil {
		return CheckSummary{}, err
	}
	status := CheckSummary{Success: len(checks) > 0}
	for _, c := range checks {
		switch c.Bucket {
		case "pass":
		case "fail", "cancel":
			status.Failed, status.Success = true, false
			status.Details += c.Name + ": " + c.State + "; "
			if c.Link != "" {
				status.FailedLinks = append(status.FailedLinks, c.Link)
			}
		default:
			status.Pending, status.Success = true, false
			status.Details += c.Name + ": " + c.Bucket + "/" + c.State + "; "
		}
	}
	return status, nil
}

func (a *App) ciStatus(ctx context.Context, pr int) (CheckSummary, error) {
	// gh pr checks returns code 8 while pending, so interpret stdout even on error.
	cmd := []string{"pr", "checks", strconv.Itoa(pr), "--json", "name,bucket,state,link"}
	args := append(cmd, "--repo", a.cfg.Repo)
	out, err := command(ctx, a.checkout, "gh", args...)
	if strings.HasPrefix(strings.TrimSpace(out), "[") {
		return ParseChecks([]byte(out))
	}
	if err != nil && (strings.Contains(strings.ToLower(err.Error()), "no checks reported") ||
		strings.Contains(strings.ToLower(err.Error()), "no checks found")) {
		return CheckSummary{Pending: true}, nil
	}
	return CheckSummary{}, err
}

func (a *App) prState(ctx context.Context, pr int) (string, error) {
	out, err := a.gh(ctx, "pr", "view", strconv.Itoa(pr), "--json", "state")
	if err != nil {
		return "", err
	}
	var state struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(out), &state); err != nil {
		return "", err
	}
	return state.State, nil
}

func (a *App) merge(ctx context.Context, pr int) error {
	if a.snapshot().MergePolicy != "auto" {
		return errors.New("merge_policy=manual forbids merge")
	}
	_, err := a.gh(ctx, "pr", "merge", strconv.Itoa(pr), "--squash")
	return err
}
