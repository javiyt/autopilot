package autopilot

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The Pi GitHub Delivery graph. Only the three reasoning stages and the
// investigation synthesis need a model. Every other stage is owned by Go.
var deliveryStages = []string{
	"repository_setup", "investigate", "plan", "implement",
	"local_verify", "review", "publish", "ci", "merge", "report",
}

func canonicalReport(stage string) string {
	switch stage {
	case "repository_setup":
		return "repository-setup.md"
	case "investigate":
		return "context.md"
	case "implement":
		return "implementation.md"
	case "local_verify":
		return "local-checks.md"
	default:
		return stage + ".md"
	}
}

func (a *App) prepareRepository(ctx context.Context) (string, string, error) {
	metadata, err := a.gh(ctx, "repo", "view", "--json", "nameWithOwner,defaultBranchRef")
	if err != nil {
		return "", "", err
	}
	var repo struct {
		NameWithOwner    string `json:"nameWithOwner"`
		DefaultBranchRef struct {
			Name string `json:"name"`
		} `json:"defaultBranchRef"`
	}
	if err := json.Unmarshal([]byte(metadata), &repo); err != nil {
		return "", "", err
	}
	if !strings.EqualFold(repo.NameWithOwner, a.cfg.Repo) || repo.DefaultBranchRef.Name == "" {
		return "", "", errors.New("GitHub repository identity or default branch could not be verified")
	}
	remote, err := command(ctx, a.checkout, "git", "remote", "get-url", "origin")
	if err != nil {
		return "", "", err
	}
	remote = strings.TrimSuffix(strings.TrimSpace(remote), ".git")
	remote = strings.TrimSuffix(remote, "/")
	reportRemote := remote
	if parsed, parseErr := url.Parse(reportRemote); parseErr == nil && parsed.User != nil {
		parsed.User = nil
		reportRemote = parsed.String()
	}
	identity := strings.ToLower(a.cfg.Repo)
	if !strings.HasSuffix(strings.ToLower(remote), "/"+identity) &&
		!strings.HasSuffix(strings.ToLower(remote), ":"+identity) {
		return "", "", fmt.Errorf("origin %q does not match configured repository %s", remote, a.cfg.Repo)
	}
	before, err := command(ctx, a.checkout, "git", "status", "--porcelain=v1")
	if err != nil {
		return "", "", err
	}
	if strings.TrimSpace(before) != "" {
		return "", "", errors.New("repository_setup refused a dirty checkout; preserve the user's work")
	}
	from, err := command(ctx, a.checkout, "git", "branch", "--show-current")
	if err != nil {
		return "", "", err
	}
	if _, err = command(ctx, a.checkout, "git", "fetch", "origin", "--prune"); err != nil {
		return "", "", err
	}
	base := repo.DefaultBranchRef.Name
	current := strings.TrimSpace(from)
	if current != base {
		if _, err = command(ctx, a.checkout, "git", "show-ref", "--verify", "--quiet", "refs/heads/"+base); err == nil {
			_, err = command(ctx, a.checkout, "git", "switch", base)
		} else {
			_, err = command(ctx, a.checkout, "git", "switch", "-c", base, "--track", "origin/"+base)
		}
		if err != nil {
			return "", "", err
		}
	}
	if _, err = command(ctx, a.checkout, "git", "merge", "--ff-only", "origin/"+base); err != nil {
		return "", "", err
	}
	baseSHA, err := command(ctx, a.checkout, "git", "rev-parse", "HEAD")
	if err != nil {
		return "", "", err
	}
	// Prune only local branches this supervisor created and Git confirms merged.
	merged, err := command(ctx, a.checkout, "git", "branch", "--merged", "origin/"+base, "--format=%(refname:short)")
	if err != nil {
		return "", "", err
	}
	var removed, retained []string
	for _, candidate := range strings.Split(merged, "\n") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || candidate == base {
			continue
		}
		if strings.HasPrefix(candidate, "autopilot/issue-") {
			if _, err := command(ctx, a.checkout, "git", "branch", "-d", candidate); err != nil {
				retained = append(retained, candidate+" (delete declined)")
			} else {
				removed = append(removed, candidate)
			}
		} else {
			retained = append(retained, candidate+" (unrelated)")
		}
	}
	report := fmt.Sprintf("# Repository setup\n\nRepository: %s\nOrigin: %s\nOriginal branch: %s\nDefault branch: %s\nBase HEAD: %s\nInitial dirty state: none\nActions: git fetch origin --prune; git switch; git merge --ff-only\nSafe local merged orphan cleanup: %v\nRetained branches: %v\nCompleted: %s\n",
		a.cfg.Repo, reportRemote, current, base, strings.TrimSpace(baseSHA), removed, retained, time.Now().UTC().Format(time.RFC3339))
	return base, report, nil
}

// Collect GitHub and repository facts with gh/git only. Pi receives filenames,
// analyzes dependencies and code paths, and creates the narrative context.md.
func (a *App) collectInvestigation(ctx context.Context, issue Issue) (string, error) {
	fields := "number,title,body,labels,author,assignees,comments,state,url,projectItems"
	issueJSON, err := a.gh(ctx, "issue", "view", strconv.Itoa(issue.Number), "--json", fields)
	if err != nil {
		return "", err
	}
	var live struct {
		State string `json:"state"`
	}
	if err := json.Unmarshal([]byte(issueJSON), &live); err != nil {
		return "", err
	}
	if live.State != "OPEN" {
		return "", fmt.Errorf("issue #%d is not open: %s", issue.Number, live.State)
	}
	if err = a.storeArtifact(issue.Number, "issue.json", issueJSON); err != nil {
		return "", err
	}
	path := fmt.Sprintf("repos/%s/issues/%d/timeline", a.cfg.Repo, issue.Number)
	timeline, timelineErr := command(ctx, "", "gh", "api", "--paginate", path)
	if timelineErr == nil {
		_ = a.storeArtifact(issue.Number, "timeline.json", timeline)
	}
	prs, prErr := a.gh(ctx, "pr", "list", "--state", "all", "--search", "#"+strconv.Itoa(issue.Number), "--limit", "100", "--json", "number,title,body,state,headRefName,baseRefName,mergedAt,url")
	if prErr == nil {
		_ = a.storeArtifact(issue.Number, "related-prs.json", prs)
	}
	files, err := command(ctx, a.checkout, "git", "ls-files")
	if err != nil {
		return "", err
	}
	if err := a.storeArtifact(issue.Number, "tracked-files.txt", files); err != nil {
		return "", err
	}
	var conventions strings.Builder
	for _, name := range []string{"AGENTS.md", "README.md", "Makefile", "package.json", "go.mod", "pyproject.toml", "Cargo.toml"} {
		data, readErr := os.ReadFile(filepath.Join(a.checkout, name))
		if readErr == nil {
			conventions.WriteString("\n### " + name + "\n" + truncate(string(data), 8000) + "\n")
		}
	}
	if err := a.storeArtifact(issue.Number, "repository-conventions.md", conventions.String()); err != nil {
		return "", err
	}
	// Establish the baseline BEFORE the implementation branch. The executor
	// performs tests directly; an LLM is not spent launching shell commands.
	baseline := make([]BaselineCheck, 0, len(a.cfg.CheckCommands))
	var baselineReport strings.Builder
	baselineReport.WriteString("# Baseline local checks (before implementation; no LLM)\n\n")
	for i, cmd := range a.cfg.CheckCommands {
		started := time.Now().UTC()
		checkCtx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		output, checkErr := command(checkCtx, a.checkout, "sh", "-c", cmd)
		cancel()
		logName := fmt.Sprintf("checks/baseline-%d-%s.log", i+1, started.Format("20060102T150405.000000000"))
		result := fmt.Sprintf("Command: %s\nStarted: %s\nOutput:\n%s\n", cmd, started.Format(time.RFC3339), output)
		if checkErr != nil {
			result += "Failure: " + checkErr.Error() + "\n"
		}
		if err := a.storeArtifact(issue.Number, logName, result); err != nil {
			return "", err
		}
		baseline = append(baseline, BaselineCheck{Command: cmd, Failed: checkErr != nil, Log: a.artifact(issue.Number, logName)})
		fmt.Fprintf(&baselineReport, "- `%s`: %s (log: %s)\n", cmd, map[bool]string{true: "BASELINE FAILED", false: "BASELINE PASS"}[checkErr != nil], a.artifact(issue.Number, logName))
	}
	if err := atomicJSON(a.artifact(issue.Number, "baseline.json"), baseline); err != nil {
		return "", err
	}
	if err := a.storeArtifact(issue.Number, "baseline-checks.md", baselineReport.String()); err != nil {
		return "", err
	}
	statusAfter, err := command(ctx, a.checkout, "git", "status", "--porcelain=v1")
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(statusAfter) != "" {
		return "", errors.New("baseline checks modified the source checkout; clean up explicitly instead of discarding changes")
	}
	// Optional endpoints can fail because of repository visibility or gh version.
	warnings := []string{}
	if timelineErr != nil {
		warnings = append(warnings, "timeline unavailable: "+truncate(timelineErr.Error(), 200))
	}
	if prErr != nil {
		warnings = append(warnings, "related PR search unavailable: "+truncate(prErr.Error(), 200))
	}
	index := fmt.Sprintf("Issue: %s\nIssue JSON: %s\nTimeline: %s\nRelated PRs: %s\nTracked files: %s\nRepository conventions: %s\nBaseline checks: %s\nWarnings: %s\n",
		issue.URL, a.artifact(issue.Number, "issue.json"), a.artifact(issue.Number, "timeline.json"), a.artifact(issue.Number, "related-prs.json"), a.artifact(issue.Number, "tracked-files.txt"), a.artifact(issue.Number, "repository-conventions.md"), a.artifact(issue.Number, "baseline-checks.md"), strings.Join(warnings, "; "))
	if err := a.storeArtifact(issue.Number, "investigation-index.md", index); err != nil {
		return "", err
	}
	return index, nil
}

type BaselineCheck struct {
	Command string `json:"command"`
	Failed  bool   `json:"failed"`
	Log     string `json:"log"`
}

// Fingerprint the complete changed file set relative to the recorded base,
// including unstaged, staged and untracked content. Unlike HEAD, this hash
// remains meaningful before source changes are committed.
func (a *App) diffFingerprint(ctx context.Context) (string, error) {
	base := a.snapshot().BaseBranch
	if base == "" {
		return "", errors.New("missing default base branch")
	}
	baseSHA, err := command(ctx, a.checkout, "git", "rev-parse", "origin/"+base)
	if err != nil {
		return "", err
	}
	tracked, err := command(ctx, a.checkout, "git", "diff", "--name-only", "--no-renames", "origin/"+base)
	if err != nil {
		return "", err
	}
	untracked, err := command(ctx, a.checkout, "git", "ls-files", "--others", "--exclude-standard")
	if err != nil {
		return "", err
	}
	names := map[string]bool{}
	for _, list := range []string{tracked, untracked} {
		for _, raw := range strings.Split(list, "\n") {
			name := strings.TrimSpace(raw)
			if name != "" {
				names[name] = true
			}
		}
	}
	ordered := make([]string, 0, len(names))
	for name := range names {
		ordered = append(ordered, name)
	}
	sort.Strings(ordered)
	hash := sha256.New()
	_, _ = hash.Write([]byte("base=" + strings.TrimSpace(baseSHA) + "\n"))
	for _, name := range ordered {
		if filepath.IsAbs(name) || name == ".." || strings.HasPrefix(name, "../") || strings.Contains(name, "\x00") {
			return "", fmt.Errorf("unsafe changed path %q", name)
		}
		full := filepath.Join(a.checkout, name)
		info, err := os.Lstat(full)
		var content []byte
		if errors.Is(err, os.ErrNotExist) {
			content = []byte("<deleted>")
		} else if err != nil {
			return "", err
		} else if info.Mode()&os.ModeSymlink != 0 {
			link, err := os.Readlink(full)
			if err != nil {
				return "", err
			}
			content = []byte("symlink:" + link)
		} else if info.Mode().IsRegular() {
			content, err = os.ReadFile(full)
			if err != nil {
				return "", err
			}
		} else {
			return "", fmt.Errorf("unsupported changed path mode: %s", name)
		}
		perFile := sha256.Sum256(content)
		_, _ = fmt.Fprintf(hash, "%s\x00%x\n", name, perFile)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func (a *App) prBody(issue Issue) string {
	var checks []string
	for _, command := range a.cfg.CheckCommands {
		checks = append(checks, "- `"+command+"` — passed")
	}
	return fmt.Sprintf("Automated implementation of %s.\n\nCloses #%d\n\n## Verification\n%s\n\nIndependent review: approved against local file fingerprint.\nArtifacts and session logs are stored outside the repository.\n",
		issue.URL, issue.Number, strings.Join(checks, "\n"))
}

var actionsRunLink = regexp.MustCompile(`/actions/runs/([0-9]+)`)

// A failed Actions job is replayed at most once, and only if its actual failed
// log contains an operator-approved infrastructure/flakiness signature.
// Otherwise the workflow sends its evidence to a fresh implementation stage.
func (a *App) ciFailureEvidence(ctx context.Context, s State, checks CheckSummary) (string, bool, error) {
	var reports []string
	rerun := false
	seen := map[string]bool{}
	for _, link := range checks.FailedLinks {
		match := actionsRunLink.FindStringSubmatch(link)
		if len(match) != 2 || seen[match[1]] {
			continue
		}
		id := match[1]
		seen[id] = true
		logText, err := a.gh(ctx, "run", "view", id, "--log-failed")
		if err != nil {
			reports = append(reports, "Could not fetch failed run "+id+": "+err.Error())
			continue
		}
		logName := filepath.Join("checks", "ci-run-"+id+"-"+time.Now().UTC().Format("20060102T150405.000000000")+".log")
		if err := a.storeArtifact(s.Issue.Number, logName, logText); err != nil {
			return "", false, err
		}
		reports = append(reports, fmt.Sprintf("Run %s failed; log: %s", id, a.artifact(s.Issue.Number, logName)))
		if s.CIReruns[id] > 0 {
			continue
		}
		for _, pattern := range a.cfg.CIRerunPatterns {
			if strings.TrimSpace(pattern) == "" || !strings.Contains(strings.ToLower(logText), strings.ToLower(pattern)) {
				continue
			}
			// Persist the attempt BEFORE the remote action: a crash cannot loop
			// the same failing infrastructure run without limits.
			if err := a.update(func(st *State) { st.CIReruns[id]++ }); err != nil {
				return "", false, err
			}
			if _, err := a.gh(ctx, "run", "rerun", id, "--failed"); err != nil {
				return strings.Join(reports, "\n"), false, fmt.Errorf("CI rerun %s denied: %w", id, err)
			}
			if err := a.update(func(st *State) { st.CIRerunAt = time.Now().UTC() }); err != nil {
				return "", false, err
			}
			rerun = true
			reports = append(reports, "Deterministically reran failed jobs for run "+id+" once (matched: "+pattern+")")
			break
		}
	}
	return strings.Join(reports, "\n"), rerun, nil
}

func (a *App) setProjectInProgress(ctx context.Context, issue Issue) {
	if a.cfg.ProjectNumber <= 0 || a.cfg.ProjectOwner == "" || a.cfg.ProjectInProgress == "" {
		return
	}
	_, err := command(ctx, "", "gh", "project", "item-edit", strconv.Itoa(a.cfg.ProjectNumber),
		"--owner", a.cfg.ProjectOwner, "--url", issue.URL, "--field", "Status", "--value", a.cfg.ProjectInProgress)
	result := "Updated existing GitHub Project item status to " + a.cfg.ProjectInProgress
	if err != nil {
		result = "Best-effort Project update skipped: " + truncate(err.Error(), 800)
	}
	_ = a.storeArtifact(issue.Number, "project-update.txt", result)
}

type prGate struct {
	State            string     `json:"state"`
	IsDraft          bool       `json:"isDraft"`
	HeadRefOid       string     `json:"headRefOid"`
	BaseRefName      string     `json:"baseRefName"`
	Body             string     `json:"body"`
	ReviewDecision   string     `json:"reviewDecision"`
	MergeStateStatus string     `json:"mergeStateStatus"`
	MergedAt         *time.Time `json:"mergedAt"`
	MergeCommit      *struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
}

func (a *App) getPRGate(ctx context.Context, number int) (prGate, error) {
	var p prGate
	out, err := a.gh(ctx, "pr", "view", strconv.Itoa(number), "--json", "state,isDraft,headRefOid,baseRefName,body,reviewDecision,mergeStateStatus,mergedAt,mergeCommit")
	if err != nil {
		return p, err
	}
	if err = json.Unmarshal([]byte(out), &p); err != nil {
		return p, err
	}
	return p, nil
}

func (a *App) verifyPublishedPR(ctx context.Context, s State) (prGate, error) {
	p, err := a.getPRGate(ctx, s.PRNumber)
	if err != nil {
		return p, err
	}
	if p.State != "OPEN" || p.IsDraft {
		return p, fmt.Errorf("PR must be open and non-draft: state=%s draft=%t", p.State, p.IsDraft)
	}
	if p.BaseRefName != s.BaseBranch || p.HeadRefOid == "" {
		return p, errors.New("PR base/head identity differs from verified branch")
	}
	if s.PublishedSHA == "" || s.PublishedSHA != p.HeadRefOid {
		return p, errors.New("PR head changed after publication; fresh review and CI required")
	}
	if !strings.Contains("\n"+p.Body+"\n", fmt.Sprintf("\nCloses #%d\n", s.Issue.Number)) {
		return p, errors.New("PR body lacks the required standalone closes reference")
	}
	return p, nil
}

func (a *App) finalReport(ctx context.Context, s State) (string, error) {
	status, err := a.gh(ctx, "issue", "view", strconv.Itoa(s.Issue.Number), "--json", "state,title,url")
	if err != nil {
		return "", err
	}
	prStatus := "none"
	mergeStatus := "not requested"
	if s.PRNumber > 0 {
		p, err := a.getPRGate(ctx, s.PRNumber)
		if err != nil {
			return "", err
		}
		prStatus = fmt.Sprintf("#%d %s (head %s)", s.PRNumber, p.State, p.HeadRefOid)
		if p.State == "MERGED" {
			mergeStatus = "squash merged, verified by GitHub"
		} else if s.MergePolicy == "manual" {
			mergeStatus = "manual policy: PR deliberately left open for operator merge"
		} else {
			mergeStatus = "not merged; inspect saved evidence"
		}
	}
	project := strings.TrimSpace(a.readArtifact(s.Issue.Number, "project-update.txt"))
	if project == "" {
		project = "skipped (not configured)"
	}
	return fmt.Sprintf("# Final delivery report\n\nTicket: #%d %q\nIssue state: %s\nRepository: %s\nBranch: %s\nBase: %s\nPlan: %s\nLocal checks: %s\nReview: %s\nPR: %s\nCI head: %s\nMerge: %s\nProject: %s\nOrphan branches: see repository-setup.md; unrelated branches preserved\nModel sessions: fresh, artifact-only handoff\nEvidence directory: %s\n",
		s.Issue.Number, s.Issue.Title, strings.TrimSpace(status), a.cfg.Repo, branchFor(s.Issue.Number), s.BaseBranch,
		a.artifact(s.Issue.Number, "plan.md"), a.artifact(s.Issue.Number, "local-checks.md"),
		a.artifact(s.Issue.Number, "review.md"), prStatus, s.PublishedSHA, mergeStatus, project, a.artifact(s.Issue.Number, "")), nil
}
