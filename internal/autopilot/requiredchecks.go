package autopilot

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// Unreported required status checks (issue #42). A required status check on
// the base branch whose check name the current CI no longer produces (a
// previous PR renamed a workflow job) can never report: GitHub holds the PR
// merge gate at BLOCKED forever while the check shows "Waiting for status to
// be reported". `gh pr checks` lists only runs that exist, so the ci phase
// sees green and the merge phase dies with a generic gate error. This module
// closes that gap: it compares the base branch's required contexts against
// the check runs reported for the PR head, and after a grace window applies
// bounded remediation — one dispatch per dispatchable workflow, one ruleset
// rewrite per head SHA for stale contexts, and a specific block otherwise.
// Security invariants: a rewrite only removes contexts that no workflow on
// the head produces and only adds job names that the head's pull_request CI
// actually reports; every other ruleset rule is preserved verbatim; dispatch
// pins the exact 40-hex head SHA (never a branch or tag); no elevated
// permissions are used.

// fullCommitSHA bounds what may be pinned as a dispatch ref (same invariant
// as the remote-check runner: a branch/tag is never dispatched).
var fullCommitSHA = regexp.MustCompile(`^[0-9a-f]{40}$`)

// workflowProducer describes the check runs one workflow file can produce on
// a pull request head: the workflow display name (its summary run) and one
// check context per job — the job's `name:` when set, otherwise the job key.
// Triggers lists the top-level `on:` keys.
type workflowProducer struct {
	File     string
	Name     string
	Jobs     []string
	Triggers []string
}

func (p workflowProducer) hasTrigger(t string) bool {
	for _, tr := range p.Triggers {
		if tr == t {
			return true
		}
	}
	return false
}

// produces reports whether a check run with the given context can come from
// this workflow: a job check name or the workflow's own summary run name.
func (p workflowProducer) produces(context string) bool {
	if p.Name == context {
		return true
	}
	for _, j := range p.Jobs {
		if j == context {
			return true
		}
	}
	return false
}

// scanWorkflowProducers parses the checkout's .github/workflows/*.yml|yaml
// documents and returns one producer per file. The checkout must be the PR
// branch at the published head, so the parsed definitions are exactly the
// ones GitHub executed (or would execute) for that commit.
func scanWorkflowProducers(checkout string) []workflowProducer {
	dir := filepath.Join(checkout, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var producers []workflowProducer
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yml") && !strings.HasSuffix(name, ".yaml") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			continue
		}
		var doc map[string]interface{}
		if err := yaml.Unmarshal(data, &doc); err != nil {
			continue
		}
		p := workflowProducer{File: name}
		if n, ok := doc["name"].(string); ok && strings.TrimSpace(n) != "" {
			p.Name = n
		} else {
			p.Name = strings.TrimSuffix(name, filepath.Ext(name))
		}
		switch on := doc["on"].(type) {
		case string:
			p.Triggers = []string{on}
		case []interface{}:
			for _, t := range on {
				if s, ok := t.(string); ok {
					p.Triggers = append(p.Triggers, s)
				}
			}
		case map[string]interface{}:
			for k := range on {
				p.Triggers = append(p.Triggers, k)
			}
		}
		sort.Strings(p.Triggers)
		if jobs, ok := doc["jobs"].(map[string]interface{}); ok {
			for key, v := range jobs {
				jobName := key
				if m, ok := v.(map[string]interface{}); ok {
					if n, ok := m["name"].(string); ok && strings.TrimSpace(n) != "" {
						jobName = n
					}
				}
				p.Jobs = append(p.Jobs, jobName)
			}
			sort.Strings(p.Jobs)
		}
		producers = append(producers, p)
	}
	sort.Slice(producers, func(i, j int) bool { return producers[i].File < producers[j].File })
	return producers
}

// parseRulesetRequiredContexts extracts the required status check contexts
// from a `gh api repos/{owner}/{repo}/rulesets` response. GitHub returns
// either an object ({rulesets:[...]}) or a bare array; both forms are
// handled. Only active repository rulesets whose target branch equals base
// contribute; rule parameters may be null (e.g. deletion rules).
func parseRulesetRequiredContexts(data []byte, base string) ([]string, error) {
	var wrapper struct {
		Rulesets []json.RawMessage `json:"rulesets"`
	}
	raw := [][]json.RawMessage{}
	if err := json.Unmarshal(data, &wrapper); err == nil && len(wrapper.Rulesets) > 0 {
		raw = append(raw, wrapper.Rulesets)
	} else {
		var arr []json.RawMessage
		if err := json.Unmarshal(data, &arr); err != nil {
			return nil, fmt.Errorf("unrecognized rulesets response shape: %w", err)
		}
		raw = append(raw, arr)
	}
	var out []string
	for _, r := range raw[0] {
		var rs struct {
			Target      string `json:"target"`
			Enforcement string `json:"enforcement"`
			Rules       []struct {
				Type       string `json:"type"`
				Parameters struct {
					Required []struct {
						Context string `json:"context"`
					} `json:"required_status_checks"`
				} `json:"parameters"`
			} `json:"rules"`
		}
		if err := json.Unmarshal(r, &rs); err != nil {
			continue
		}
		if rs.Target != base || rs.Enforcement != "active" {
			continue
		}
		for _, rule := range rs.Rules {
			if rule.Type != "required_status_checks" {
				continue
			}
			for _, c := range rule.Parameters.Required {
				if c.Context != "" {
					out = append(out, c.Context)
				}
			}
		}
	}
	sort.Strings(out)
	return out, nil
}

// parseProtectionRequiredContexts extracts the required check contexts from
// a classic `gh api repos/{owner}/{repo}/branches/{base}/protection` response
// (required_status_checks may be null when unused).
func parseProtectionRequiredContexts(data []byte) ([]string, error) {
	var doc struct {
		RequiredStatusChecks *struct {
			Contexts []string `json:"contexts"`
		} `json:"required_status_checks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("unrecognized branch protection response: %w", err)
	}
	if doc.RequiredStatusChecks == nil {
		return nil, nil
	}
	return doc.RequiredStatusChecks.Contexts, nil
}

// parseCheckRunContexts extracts the check-run names from a
// `gh api repos/{owner}/{repo}/commits/{sha}/check-runs` response: the set of
// contexts GitHub has actually reported for that commit.
func parseCheckRunContexts(data []byte) (map[string]bool, error) {
	var doc struct {
		CheckRuns []struct {
			Name string `json:"name"`
		} `json:"check_runs"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("unrecognized check-runs response: %w", err)
	}
	out := map[string]bool{}
	for _, c := range doc.CheckRuns {
		if c.Name != "" {
			out[c.Name] = true
		}
	}
	return out, nil
}

// checkGap is a required context with no reported run on the PR head.
// Producer is nil when no workflow on the head can produce the context —
// the stale case (renamed job, external app check, deleted workflow).
type checkGap struct {
	Context  string
	Producer *workflowProducer
}

func (g checkGap) stale() bool { return g.Producer == nil }
func (g checkGap) dispatchable() bool {
	return g.Producer != nil && g.Producer.hasTrigger("workflow_dispatch")
}
func (g checkGap) liveNonDispatchable() bool {
	return g.Producer != nil && !g.Producer.hasTrigger("workflow_dispatch")
}

// computeCheckGaps returns, in stable order, every required context with no
// reported run, each paired with the workflow producer when one exists.
func computeCheckGaps(required []string, reported map[string]bool, producers []workflowProducer) []checkGap {
	var gaps []checkGap
	seen := map[string]bool{}
	for _, c := range required {
		if reported[c] || seen[c] {
			continue
		}
		seen[c] = true
		g := checkGap{Context: c}
		for i := range producers {
			if producers[i].produces(c) {
				g.Producer = &producers[i]
				break
			}
		}
		gaps = append(gaps, g)
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].Context < gaps[j].Context })
	return gaps
}

// newRequiredCheckSet computes the ruleset rewrite target: keep every
// required context that already reported a run on the head, and add every
// job check name of pull_request-triggering workflows on the head. The
// result is deduped and sorted, so the rewrite is deterministic and can only
// remove stale contexts and add checks the head CI actually reports.
func newRequiredCheckSet(required []string, reported map[string]bool, producers []workflowProducer) []string {
	set := map[string]bool{}
	for _, c := range required {
		if reported[c] {
			set[c] = true
		}
	}
	for _, p := range producers {
		if p.hasTrigger("pull_request") {
			for _, j := range p.Jobs {
				set[j] = true
			}
		}
	}
	var out []string
	for c := range set {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// buildRulesetRewritePayload produces the PUT body for
// `repos/{owner}/{repo}/rulesets/{id}` from the fetched ruleset document:
// the same name, target, enforcement and bypass policy, with every rule
// preserved verbatim except required_status_checks, whose
// required_status_checks list is replaced by newContexts (each pinned to the
// GitHub Actions integration id 15368).
func buildRulesetRewritePayload(doc []byte, newContexts []string) ([]byte, error) {
	var rs struct {
		Name         string            `json:"name"`
		Target       string            `json:"target"`
		Enforcement  string            `json:"enforcement"`
		BypassMode   string            `json:"bypass_mode"`
		BypassActors []json.RawMessage `json:"bypass_actors"`
		Rules        []json.RawMessage `json:"rules"`
	}
	if err := json.Unmarshal(doc, &rs); err != nil {
		return nil, fmt.Errorf("unrecognized ruleset document: %w", err)
	}
	if rs.Target == "" || rs.Name == "" {
		return nil, fmt.Errorf("ruleset document is missing name or target")
	}
	type paramEntry struct {
		Context       string `json:"context"`
		IntegrationID int    `json:"integration_id"`
	}
	var paramsList []paramEntry
	for _, c := range newContexts {
		paramsList = append(paramsList, paramEntry{Context: c, IntegrationID: 15368})
	}
	var rules []json.RawMessage
	sawRequired := false
	for _, raw := range rs.Rules {
		var rule struct {
			Type       string                 `json:"type"`
			Parameters map[string]interface{} `json:"parameters"`
		}
		if err := json.Unmarshal(raw, &rule); err != nil {
			continue
		}
		if rule.Type == "required_status_checks" {
			sawRequired = true
			// Keep the rule's other parameters (strict_required_status_checks_policy,
			// do_not_enforce_on_create, ...) verbatim; replace only the list.
			if rule.Parameters == nil {
				rule.Parameters = map[string]interface{}{}
			}
			rule.Parameters["required_status_checks"] = paramsList
			out, err := json.Marshal(rule)
			if err != nil {
				return nil, err
			}
			rules = append(rules, out)
			continue
		}
		rules = append(rules, raw)
	}
	if !sawRequired {
		rule := struct {
			Type       string                 `json:"type"`
			Parameters map[string]interface{} `json:"parameters"`
		}{
			Type:       "required_status_checks",
			Parameters: map[string]interface{}{"required_status_checks": paramsList},
		}
		out, err := json.Marshal(rule)
		if err != nil {
			return nil, err
		}
		rules = append(rules, out)
	}
	payload := struct {
		Name         string            `json:"name"`
		Target       string            `json:"target"`
		Enforcement  string            `json:"enforcement"`
		BypassMode   string            `json:"bypass_mode,omitempty"`
		BypassActors []json.RawMessage `json:"bypass_actors,omitempty"`
		Rules        []json.RawMessage `json:"rules"`
	}{rs.Name, rs.Target, rs.Enforcement, rs.BypassMode, rs.BypassActors, rules}
	return json.Marshal(payload)
}

type remediationAction int

const (
	remediationNone remediationAction = iota
	remediationWaiting
	remediationDispatched
	remediationHealed
	remediationBlock
)

type gapDecision struct {
	Action remediationAction
	Note   string
}

func (d gapDecision) blockError() error {
	if d.Action != remediationBlock {
		return nil
	}
	return fmt.Errorf("%s", d.Note)
}

// decideGapRemediation classifies the gaps against the persisted attempt
// state and decides the bounded remediation. dispatched maps the contexts
// already dispatched this issue run; healedSHA is the head SHA for which a
// ruleset rewrite already happened; autoFix enables dispatch and rewrite;
// capElapsed means the whole CI wait window (ci_wait_minutes) has passed,
// which turns any remaining waiting into a specific block. The decision is
// pure so every branch is unit-testable without a network.
func decideGapRemediation(gaps []checkGap, dispatched map[string]bool, healedSHA, headSHA string, autoFix, graceElapsed, capElapsed bool) gapDecision {
	if len(gaps) == 0 {
		return gapDecision{Action: remediationNone}
	}
	var names []string
	for _, g := range gaps {
		names = append(names, g.Context)
	}
	joined := strings.Join(names, ", ")
	if !graceElapsed {
		if capElapsed {
			return gapDecision{Action: remediationBlock, Note: fmt.Sprintf("required check(s) never reported within the CI wait window: %s; %s", joined, capGuidance(gaps, dispatched))}
		}
		return gapDecision{Action: remediationWaiting, Note: fmt.Sprintf("%d required check(s) unreported (%s); within the grace window, waiting for the runs to appear", len(gaps), joined)}
	}
	hasStale := false
	var toDispatch []string
	for _, g := range gaps {
		switch {
		case g.stale():
			hasStale = true
		case g.dispatchable() && !dispatched[g.Context]:
			toDispatch = append(toDispatch, g.Context+" via "+g.Producer.File)
		}
	}
	if !autoFix {
		if hasStale {
			return gapDecision{Action: remediationBlock, Note: fmt.Sprintf("required check(s) never reported: %s; no workflow on the PR head produces them and automatic remediation is disabled (required_check_auto_fix); update the base branch's required status checks or enable auto-fix", joined)}
		}
		return gapDecision{Action: remediationBlock, Note: fmt.Sprintf("required check(s) never reported: %s; automatic remediation is disabled (required_check_auto_fix); trigger the producing workflow or update the ruleset", joined)}
	}
	if hasStale {
		if healedSHA == headSHA {
			return gapDecision{Action: remediationBlock, Note: fmt.Sprintf("ruleset already rewritten for head %s but required check(s) still unreported: %s; inspect the Actions runs and the base branch ruleset", headSHA[:12], joined)}
		}
		return gapDecision{Action: remediationHealed, Note: fmt.Sprintf("stale required check(s) (no workflow on the head produces them): %s; rewriting the base branch required status checks to the checks the head CI actually reports", joined)}
	}
	if len(toDispatch) > 0 {
		return gapDecision{Action: remediationDispatched, Note: fmt.Sprintf("dispatching workflow(s) for unreported required check(s): %s (pinned to head %s)", strings.Join(toDispatch, "; "), headSHA[:12])}
	}
	if capElapsed {
		return gapDecision{Action: remediationBlock, Note: fmt.Sprintf("required check(s) still unreported after the CI wait window: %s; %s", joined, capGuidance(gaps, dispatched))}
	}
	return gapDecision{Action: remediationWaiting, Note: fmt.Sprintf("required check(s) produced by the head's workflows have not reported yet: %s; waiting for the runs", joined)}
}

// capGuidance turns the remaining (non-stale, non-dispatchable-or-already-
// dispatched) gaps into per-context operator guidance for the cap block.
func capGuidance(gaps []checkGap, dispatched map[string]bool) string {
	var parts []string
	for _, g := range gaps {
		switch {
		case g.stale():
			parts = append(parts, fmt.Sprintf("%s: no workflow on the head produces it — update the base branch's required status checks", g.Context))
		case g.dispatchable() && dispatched[g.Context]:
			parts = append(parts, fmt.Sprintf("%s: the workflow was already dispatched but the run did not report — inspect the Actions queue for %s", g.Context, g.Producer.File))
		case g.dispatchable():
			parts = append(parts, fmt.Sprintf("%s: trigger %s (workflow_dispatch) on the PR head manually", g.Context, g.Producer.File))
		default:
			parts = append(parts, fmt.Sprintf("%s: %s does not declare workflow_dispatch and the run has not reported — trigger it or update the ruleset", g.Context, g.Producer.File))
		}
	}
	return strings.Join(parts, "; ")
}

// requiredCheckContextsForBase returns the required status check contexts for
// the base branch: active rulesets first, with classic branch protection as
// the fallback for repositories that predate rulesets.
func (a *App) requiredCheckContextsForBase(ctx context.Context, base string) ([]string, error) {
	out, err := command(ctx, a.checkout, "gh", "api", "repos/"+a.cfg.Repo+"/rulesets")
	if err != nil {
		return nil, err
	}
	contexts, err := parseRulesetRequiredContexts([]byte(out), base)
	if err != nil {
		return nil, err
	}
	if len(contexts) > 0 {
		return contexts, nil
	}
	// No active ruleset requires checks: try classic branch protection.
	out, err = command(ctx, a.checkout, "gh", "api", "repos/"+a.cfg.Repo+"/branches/"+base+"/protection")
	if err != nil {
		// 404 = branch not protected: legitimately no required checks.
		if strings.Contains(err.Error(), "404") {
			return nil, nil
		}
		return nil, err
	}
	return parseProtectionRequiredContexts([]byte(out))
}

// reportedCheckContexts returns the set of check contexts GitHub has
// reported for the given commit (paginated check-runs API, first page capped
// at 100 — enough for any realistic CI fan-out here).
func (a *App) reportedCheckContexts(ctx context.Context, sha string) (map[string]bool, error) {
	out, err := command(ctx, a.checkout, "gh", "api", "repos/"+a.cfg.Repo+"/commits/"+sha+"/check-runs?per_page=100")
	if err != nil {
		return nil, err
	}
	return parseCheckRunContexts([]byte(out))
}

// dispatchCheckWorkflow triggers a same-repo workflow pinned to the exact
// head SHA via workflow dispatch. The ref must be a full 40-hex commit SHA;
// a branch or tag is never dispatched (same invariant as remotechecks.go).
func (a *App) dispatchCheckWorkflow(ctx context.Context, file, ref string) error {
	if !fullCommitSHA.MatchString(ref) {
		return fmt.Errorf("refusing to dispatch %s: ref %q is not a full commit SHA", file, ref)
	}
	out, err := command(ctx, a.checkout, "gh", "run", "create", file, "--ref", ref, "--repo", a.cfg.Repo)
	if err != nil {
		return fmt.Errorf("gh run create %s --ref %s failed: %w", file, ref, err)
	}
	if id := strings.TrimSpace(out); id != "" {
		log.Printf("required-check remediation: dispatched %s at %s (run %s)", file, ref[:12], id)
	}
	return nil
}

// baseRulesetRef is one entry of the `gh api repos/{repo}/rulesets` list
// (either the object's rulesets array or a bare array).
type baseRulesetRef struct {
	ID          int    `json:"id"`
	Target      string `json:"target"`
	Enforcement string `json:"enforcement"`
}

// rewriteRequiredChecks rewrites the active base-branch ruleset's
// required_status_checks to newContexts, preserving every other rule and the
// bypass policy verbatim. It returns the previous context list. Only an
// active branch ruleset with a required_status_checks rule is eligible.
func (a *App) rewriteRequiredChecks(ctx context.Context, base string, newContexts []string) ([]string, error) {
	listOut, err := command(ctx, a.checkout, "gh", "api", "repos/"+a.cfg.Repo+"/rulesets")
	if err != nil {
		return nil, err
	}
	var refs []baseRulesetRef
	var wrapper struct {
		Rulesets []baseRulesetRef `json:"rulesets"`
	}
	if err := json.Unmarshal([]byte(listOut), &wrapper); err == nil {
		refs = wrapper.Rulesets
	} else if err := json.Unmarshal([]byte(listOut), &refs); err != nil {
		return nil, fmt.Errorf("unrecognized rulesets list: %w", err)
	}
	var id int
	found := false
	for _, r := range refs {
		if r.Target == base && r.Enforcement == "active" {
			id = r.ID
			found = true
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("no active ruleset targets branch %q; refusing to guess where required checks live", base)
	}
	docOut, err := command(ctx, a.checkout, "gh", "api", fmt.Sprintf("repos/%s/rulesets/%d", a.cfg.Repo, id))
	if err != nil {
		return nil, err
	}
	var probe struct {
		Rules []struct {
			Type string `json:"type"`
		} `json:"rules"`
	}
	if err := json.Unmarshal([]byte(docOut), &probe); err != nil {
		return nil, fmt.Errorf("unrecognized ruleset document: %w", err)
	}
	sawRequired := false
	for _, r := range probe.Rules {
		if r.Type == "required_status_checks" {
			sawRequired = true
			break
		}
	}
	if !sawRequired {
		return nil, fmt.Errorf("ruleset %d has no required_status_checks rule; nothing to rewrite", id)
	}
	payload, err := buildRulesetRewritePayload([]byte(docOut), newContexts)
	if err != nil {
		return nil, err
	}
	tmp, err := os.CreateTemp("", "autopilot-ruleset-*.json")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		return nil, err
	}
	tmp.Close()
	old, _ := a.requiredCheckContextsForBase(ctx, base)
	if _, err := command(ctx, a.checkout, "gh", "api", "--method", "PUT", "--input", tmp.Name(), fmt.Sprintf("repos/%s/rulesets/%d", a.cfg.Repo, id)); err != nil {
		return nil, fmt.Errorf("ruleset update denied or failed: %w", err)
	}
	return old, nil
}

// remediateUnreportedRequiredChecks detects required status checks for the
// PR base that have no reported run on the PR head and applies the bounded
// remediation: grace window, one dispatch per dispatchable context per issue
// run, one ruleset rewrite per head SHA. The returned action drives the
// caller: waiting/dispatched/healed mean keep polling (the handler waits one
// poll); block is returned as a specific error. Detection API failures are
// logged, not fatal: the existing ci_wait_minutes bound still applies.
func (a *App) remediateUnreportedRequiredChecks(ctx context.Context, s State, p prGate) (remediationAction, string, error) {
	if s.PRNumber <= 0 || p.HeadRefOid == "" || p.BaseRefName == "" {
		return remediationNone, "", nil
	}
	required, err := a.requiredCheckContextsForBase(ctx, p.BaseRefName)
	if err != nil {
		log.Printf("required-check detection: ruleset lookup failed: %v", err)
		return remediationNone, "", nil
	}
	if len(required) == 0 {
		return remediationNone, "", nil
	}
	reported, err := a.reportedCheckContexts(ctx, p.HeadRefOid)
	if err != nil {
		log.Printf("required-check detection: check-runs lookup failed: %v", err)
		return remediationNone, "", nil
	}
	// The checkout is the dedicated issue branch at the published head
	// (verifyPublishedPR bound s.PublishedSHA to p.HeadRefOid), so its
	// workflow definitions are the ones GitHub used for this commit.
	producers := scanWorkflowProducers(a.checkout)
	gaps := computeCheckGaps(required, reported, producers)
	grace := time.Duration(a.cfg.RequiredCheckGraceMinutes) * time.Minute
	if grace <= 0 {
		grace = 5 * time.Minute
	}
	ciWait := time.Duration(a.cfg.CIWaitMinutes) * time.Minute
	if ciWait <= 0 {
		ciWait = 30 * time.Minute
	}
	graceElapsed := s.CIStartedAt.IsZero() || time.Since(s.CIStartedAt) >= grace
	capElapsed := !s.CIStartedAt.IsZero() && time.Since(s.CIStartedAt) >= ciWait
	dispatched := map[string]bool{}
	for c := range s.RequiredCheckDispatches {
		dispatched[c] = true
	}
	decision := decideGapRemediation(gaps, dispatched, s.RequiredCheckHealedForSHA, p.HeadRefOid, a.cfg.requiredCheckAutoFixEnabled(), graceElapsed, capElapsed)
	if decision.Action == remediationNone || decision.Action == remediationWaiting {
		return decision.Action, decision.Note, nil
	}
	switch decision.Action {
	case remediationDispatched:
		for _, g := range gaps {
			if !g.dispatchable() || dispatched[g.Context] {
				continue
			}
			if err := a.dispatchCheckWorkflow(ctx, g.Producer.File, p.HeadRefOid); err != nil {
				return remediationBlock, decision.Note, err
			}
			dispatched[g.Context] = true
			c := g.Context
			// Record each dispatch immediately so a later failure in the loop
			// cannot make the next tick re-dispatch what already succeeded.
			if err := a.update(func(st *State) {
				if st.RequiredCheckDispatches == nil {
					st.RequiredCheckDispatches = map[string]time.Time{}
				}
				st.RequiredCheckDispatches[c] = time.Now().UTC()
			}); err != nil {
				return remediationNone, "", err
			}
		}
		a.notify(fmt.Sprintf("Issue #%d: %s", s.Issue.Number, decision.Note))
		return decision.Action, decision.Note, nil
	case remediationHealed:
		newSet := newRequiredCheckSet(required, reported, producers)
		if len(newSet) == 0 {
			return remediationBlock, decision.Note, fmt.Errorf("required check(s) never reported: %s; no pull_request-triggering CI exists on the PR head to derive required checks from; update the base branch ruleset manually", strings.Join(namesOf(gaps), ", "))
		}
		old, err := a.rewriteRequiredChecks(ctx, p.BaseRefName, newSet)
		if err != nil {
			return remediationBlock, decision.Note, fmt.Errorf("required check(s) never reported: %s; ruleset rewrite failed: %v", strings.Join(namesOf(gaps), ", "), err)
		}
		if err := a.update(func(st *State) { st.RequiredCheckHealedForSHA = p.HeadRefOid }); err != nil {
			return remediationNone, "", err
		}
		note := fmt.Sprintf("required-check remediation: %s — previous required: [%s], new required: [%s]", decision.Note, strings.Join(old, ", "), strings.Join(newSet, ", "))
		a.notify(fmt.Sprintf("Issue #%d: stale required status check(s) removed from the %s ruleset; required checks are now: %s", s.Issue.Number, p.BaseRefName, strings.Join(newSet, ", ")))
		return decision.Action, note, nil
	default:
		return decision.Action, decision.Note, decision.blockError()
	}
}

func namesOf(gaps []checkGap) []string {
	var out []string
	for _, g := range gaps {
		out = append(out, g.Context)
	}
	return out
}
