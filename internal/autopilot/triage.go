package autopilot

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// QueueIssue is one open issue as collected by the deterministic triage
// stage. The LLM only reorders these facts; it cannot add or remove work.
type QueueIssue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
	URL       string    `json:"url"`
	Milestone struct {
		Title string `json:"title"`
	} `json:"milestone"`
	Labels []struct {
		Name string `json:"name"`
	} `json:"labels"`
}

// triageLLMCap bounds the issues offered to the ordering model. Beyond it the
// digest no longer fits a context window, so the deterministic order wins.
const triageLLMCap = 200

// collectQueue lists every eligible open issue with gh only. An empty
// milestone or label filter means "every open issue of the repository".
func (a *App) collectQueue(ctx context.Context) ([]QueueIssue, error) {
	args := []string{"issue", "list", "--state", "open", "--limit", "1000", "--json", "number,title,body,url,createdAt,milestone,labels"}
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
	var issues []QueueIssue
	if err := json.Unmarshal([]byte(out), &issues); err != nil {
		return nil, fmt.Errorf("parse issue list: %w", err)
	}
	return issues, nil
}

type depRef struct {
	target int
	// "before" means this issue must be implemented before target; "after"
	// means target must be implemented before this issue.
	dir string
}

var (
	depsBefore = regexp.MustCompile(`(?i)\b(?:blocks?|unblocks?|prerequisite\s+(?:for|of)|must\s+land\s+before|blocked\s+by)\s+(?:issue\s+|gh\s+)?#?(\d{1,6})\b`)
	depsAfter  = regexp.MustCompile(`(?i)\bdepends\s+on\b\s+(?:issue\s+|gh\s+)?#?(\d{1,6})\b`)
	// List forms such as "Dependencies: #12, #34" or a markdown-bolded
	// "**Dependencies:** HS-045(#64)". Only references carrying a # count,
	// so ticket numbers like HS-006 stay out of the graph. The captured list
	// stops at the next bold marker or a different keyword, so a trailing
	// "**Related:** #8" segment is never mistaken for a dependency.
	depsAfterList  = regexp.MustCompile(`(?im)^[ \t]*(?:[-*+][ \t]+)?(?:\*\*|__)?(?:prerequisites?|dependencies?|blocked[ \t]+by)\s*[*]*\s*:\s*[*]*\s*([^\n]*)`)
	depsBeforeList = regexp.MustCompile(`(?im)^[ \t]*(?:[-*+][ \t]+)?(?:\*\*|__)?blocks?\s*[*]*\s*:\s*[*]*\s*([^\n]*)`)
	depListRef     = regexp.MustCompile(`#(\d{1,6})\b`)
	// depListEnd cuts a dependency list at the next bold marker or at a
	// different keyword, so a trailing "**Related:** #8" segment on the same
	// line is never mistaken for a dependency.
	depListEnd = regexp.MustCompile(`\s*(?:\*\*|__)\s*|\s+(?:related|blocks?|dependencies?|prerequisites?)\s*:`)
)

// parseIssueDeps extracts deterministic dependency hints from an issue body.
func parseIssueDeps(body string) []depRef {
	var refs []depRef
	seen := map[depRef]bool{}
	add := func(target int, dir string) {
		if target <= 0 {
			return
		}
		ref := depRef{target: target, dir: dir}
		if !seen[ref] {
			seen[ref] = true
			refs = append(refs, ref)
		}
	}
	listForms := map[*regexp.Regexp]string{
		depsAfterList:  "after",
		depsBeforeList: "before",
	}
	patterns := []struct {
		re  *regexp.Regexp
		dir string
	}{
		{depsBefore, "before"},
		{depsAfter, "after"},
	}
	for _, p := range patterns {
		for _, m := range p.re.FindAllStringSubmatch(body, -1) {
			if n, err := strconv.Atoi(m[1]); err == nil {
				add(n, p.dir)
			}
		}
	}
	for re, dir := range listForms {
		for _, m := range re.FindAllStringSubmatch(body, -1) {
			segment := depListEnd.Split(m[1], 2)[0]
			for _, ref := range depListRef.FindAllStringSubmatch(segment, -1) {
				if n, err := strconv.Atoi(ref[1]); err == nil {
					add(n, dir)
				}
			}
		}
	}
	return refs
}

// ---- Ordering rules ----
//
// The queue sorts by a four-level rank: open bugs first, then milestone
// version from the lowest to the highest (v0.1 before v0.2), then the
// priority label P0..P2, then creation order. Issues without a milestone sit
// after every versioned milestone unless the LLM assigns them a version.
// Dependency edges always win over the rank: a blocked issue never moves
// ahead of its blocker, whatever the rank says.

var (
	bugLabelRe      = regexp.MustCompile(`(?i)^(?:type:)?bug$`)
	priorityLabelRe = regexp.MustCompile(`(?i)^priority:p?([0-9])$`)
)

// issueFact is one issue reduced to the labels the ordering rules read.
type issueFact struct {
	number   int
	bug      bool
	priority int // 0 = P0, 1 = P1, 2 = P2, 3 = no priority label
	version  []int
}

func factFor(issue QueueIssue) issueFact {
	f := issueFact{number: issue.Number, priority: 3}
	for _, label := range issue.Labels {
		name := strings.ToLower(strings.TrimSpace(label.Name))
		if !f.bug && bugLabelRe.MatchString(name) {
			f.bug = true
		}
		if m := priorityLabelRe.FindStringSubmatch(name); m != nil {
			n, _ := strconv.Atoi(m[1])
			if n > 2 {
				n = 3
			}
			f.priority = n
		}
	}
	f.version = versionFromText(issue.Milestone.Title)
	return f
}

// versionFromText finds a version in a milestone title such as
// "v0.1 - End-to-end MVP". A v-prefixed field (v1, v0.4.2) or a dotted
// number (1.2) counts; anything else does not, so titles without a version
// sort as unversioned.
func versionFromText(text string) []int {
	for _, field := range strings.FieldsFunc(text, func(r rune) bool {
		return !(r == '.' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z'))
	}) {
		if v, ok := parseVersionField(field); ok {
			return v
		}
	}
	return nil
}

func parseVersionField(field string) ([]int, bool) {
	f := strings.ToLower(field)
	fromV := strings.HasPrefix(f, "v")
	if fromV {
		f = f[1:]
	}
	if f == "" {
		return nil, false
	}
	if !fromV && !strings.Contains(f, ".") {
		return nil, false // a bare number is not a milestone version
	}
	parts := strings.Split(f, ".")
	ver := make([]int, 0, len(parts))
	for _, part := range parts {
		if part == "" {
			return nil, false
		}
		for _, c := range part {
			if c < '0' || c > '9' {
				return nil, false
			}
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil, false
		}
		ver = append(ver, n)
	}
	return ver, true
}

// formatVersion renders a parsed version: [0 1] -> "v0.1".
func formatVersion(v []int) string {
	parts := make([]string, len(v))
	for i, p := range v {
		parts[i] = strconv.Itoa(p)
	}
	return "v" + strings.Join(parts, ".")
}

func compareVersions(a, b []int) int {
	for i := 0; i < len(a) || i < len(b); i++ {
		av, bv := 0, 0
		if i < len(a) {
			av = a[i]
		}
		if i < len(b) {
			bv = b[i]
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

// issueRank is the total order the deterministic queue uses.
type issueRank struct {
	bug int // 0 = bug, 1 = not a bug
	ver []int
	pri int
	num int
}

func (r issueRank) less(o issueRank) bool {
	if r.bug != o.bug {
		return r.bug < o.bug
	}
	switch {
	case r.ver == nil && o.ver == nil:
	case r.ver == nil:
		return false // unversioned sorts last
	case o.ver == nil:
		return true
	}
	if c := compareVersions(r.ver, o.ver); c != 0 {
		return c < 0
	}
	if r.pri != o.pri {
		return r.pri < o.pri
	}
	return r.num < o.num
}

// issueBand is the rank without creation order: the boundary the LLM is not
// allowed to cross when it refines the order.
type issueBand struct {
	bug int
	ver []int
	pri int
}

func (b issueBand) less(o issueBand) bool {
	if b.bug != o.bug {
		return b.bug < o.bug
	}
	switch {
	case b.ver == nil && o.ver == nil:
	case b.ver == nil:
		return false
	case o.ver == nil:
		return true
	}
	if c := compareVersions(b.ver, o.ver); c != 0 {
		return c < 0
	}
	return b.pri < o.pri
}

// effectiveVersion resolves the milestone version an issue sorts under: the
// GitHub milestone title, else an LLM-inferred version, else unversioned.
func effectiveVersion(issue QueueIssue, f issueFact, assign map[int][]int) []int {
	if f.version != nil {
		return f.version
	}
	return assign[issue.Number]
}

// DeterministicQueue orders the issues with the rule stack: open bugs first,
// then milestone version ascending, then priority label, then creation
// order; dependency edges always win over the rank. Cycles are broken toward
// the best-ranked remaining issue, so the function always returns a complete
// permutation of the input.
func DeterministicQueue(issues []QueueIssue) []int {
	return DeterministicQueueWithAssignments(issues, nil)
}

// DeterministicQueueWithAssignments is DeterministicQueue with LLM-inferred
// milestone versions for issues that have no milestone.
func DeterministicQueueWithAssignments(issues []QueueIssue, assign map[int][]int) []int {
	facts := make([]issueFact, len(issues))
	ranks := make([]issueRank, len(issues))
	known := make(map[int]bool, len(issues))
	for i, issue := range issues {
		facts[i] = factFor(issue)
		known[issue.Number] = true
	}
	// "before[n]" lists the issues that must be implemented before n.
	before := map[int]map[int]bool{}
	for _, issue := range issues {
		for _, ref := range parseIssueDeps(issue.Body) {
			if ref.target == issue.Number || !known[ref.target] {
				continue
			}
			var edge, blocker int
			if ref.dir == "before" {
				edge, blocker = ref.target, issue.Number
			} else {
				edge, blocker = issue.Number, ref.target
			}
			if before[edge] == nil {
				before[edge] = map[int]bool{}
			}
			before[edge][blocker] = true
		}
	}
	for i := range issues {
		bug := 1
		if facts[i].bug {
			bug = 0
		}
		ranks[i] = issueRank{
			bug: bug,
			ver: effectiveVersion(issues[i], facts[i], assign),
			pri: facts[i].priority,
			num: issues[i].Number,
		}
	}
	remaining := make(map[int]bool, len(issues))
	for _, issue := range issues {
		remaining[issue.Number] = true
	}
	queue := make([]int, 0, len(issues))
	for len(remaining) > 0 {
		var ready []int
		for i := range issues {
			n := issues[i].Number
			if !remaining[n] {
				continue
			}
			blocked := false
			for blocker := range before[n] {
				if remaining[blocker] {
					blocked = true
					break
				}
			}
			if !blocked {
				ready = append(ready, i)
			}
		}
		if len(ready) == 0 {
			// A dependency cycle: release the best-ranked remaining issue.
			for i := range issues {
				if remaining[issues[i].Number] {
					ready = []int{i}
				}
			}
		}
		best := ready[0]
		for _, i := range ready[1:] {
			if ranks[i].less(ranks[best]) {
				best = i
			}
		}
		queue = append(queue, issues[best].Number)
		delete(remaining, issues[best].Number)
	}
	return queue
}

// ---- LLM side of the plan ----

type triageAssignment struct {
	N         int    `json:"n"`
	Milestone string `json:"milestone"`
}

type triageOrderEntry struct {
	N   int    `json:"n"`
	Why string `json:"why"`
}

type triageAnswer struct {
	Assignments []triageAssignment `json:"assignments"`
	Order       []triageOrderEntry `json:"order"`
}

// QueuePlan is the planned execution order together with the facts the
// ordering used, so the generated report can be audited.
type QueuePlan struct {
	Order   []int
	FromLLM bool
	// Assign holds LLM-inferred milestone versions for issues that have no
	// milestone; the deterministic order falls back to the same assignments
	// when the LLM order is rejected.
	Assign map[int][]int
	// Why carries the LLM rationale per issue, set when the LLM order was
	// accepted.
	Why map[int]string
}

// PlanQueue turns the collected issues and the raw triage LLM answer into the
// execution order. The LLM may assign a milestone version to issues without a
// milestone and refine the order inside priority bands; crossing a band,
// violating a dependency, or dropping or duplicating an issue makes the
// deterministic order win.
func PlanQueue(issues []QueueIssue, llmText string) QueuePlan {
	assign, order, why := parseTriageAnswer(issues, llmText)
	if order != nil && orderValid(issues, assign, order) {
		return QueuePlan{Order: order, FromLLM: true, Assign: assign, Why: why}
	}
	return QueuePlan{Order: DeterministicQueueWithAssignments(issues, assign), Assign: assign}
}

// parseTriageAnswer extracts the milestone assignments and the proposed
// order from the raw LLM text. A nil order means the text did not yield a
// complete, duplicate-free permutation; assignments survive independently.
func parseTriageAnswer(issues []QueueIssue, raw string) (assign map[int][]int, order []int, why map[int]string) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil, nil
	}
	text := strings.TrimSpace(raw)
	if strings.HasPrefix(text, "```") {
		if nl := strings.Index(text, "\n"); nl >= 0 {
			text = text[nl+1:]
		}
		text = strings.TrimSuffix(strings.TrimSpace(text), "```")
		text = strings.TrimSpace(text)
	}
	known := make(map[int]bool, len(issues))
	needsMilestone := map[int]bool{}
	for _, issue := range issues {
		known[issue.Number] = true
		if strings.TrimSpace(issue.Milestone.Title) == "" {
			needsMilestone[issue.Number] = true
		}
	}
	setAssign := func(n int, milestone string) {
		if !known[n] || !needsMilestone[n] {
			return
		}
		v := versionFromText(milestone)
		if v == nil {
			return
		}
		if assign == nil {
			assign = map[int][]int{}
		}
		assign[n] = v
	}
	buildOrder := func(entries []triageOrderEntry) []int {
		if len(entries) != len(issues) {
			return nil
		}
		seen := map[int]bool{}
		out := make([]int, 0, len(entries))
		for _, e := range entries {
			if !known[e.N] || seen[e.N] {
				return nil
			}
			seen[e.N] = true
			out = append(out, e.N)
			if e.Why != "" {
				if why == nil {
					why = map[int]string{}
				}
				why[e.N] = e.Why
			}
		}
		return out
	}
	// Object form: optional assignments plus the full order.
	if i := strings.Index(text, "{"); i >= 0 {
		if j := strings.LastIndex(text, "}"); j > i {
			var ans triageAnswer
			if json.Unmarshal([]byte(text[i:j+1]), &ans) == nil {
				for _, e := range ans.Assignments {
					setAssign(e.N, e.Milestone)
				}
				return assign, buildOrder(ans.Order), why
			}
		}
	}
	// Legacy forms from older prompts: a bare array of numbers or of
	// {"n","why"} objects, without assignments.
	if i := strings.Index(text, "["); i >= 0 {
		if j := strings.LastIndex(text, "]"); j > i {
			var entries []triageOrderEntry
			entriesOK := json.Unmarshal([]byte(text[i:j+1]), &entries) == nil
			if !entriesOK {
				var nums []int
				if json.Unmarshal([]byte(text[i:j+1]), &nums) == nil {
					entries = make([]triageOrderEntry, len(nums))
					for k, n := range nums {
						entries[k] = triageOrderEntry{N: n}
					}
				}
			}
			if entries != nil || entriesOK {
				order = buildOrder(entries)
			}
		}
	}
	return assign, order, why
}

// orderValid checks the two contracts the LLM must keep: the sequence must
// never walk backwards across priority bands, and every parsed dependency
// edge must be satisfied.
func orderValid(issues []QueueIssue, assign map[int][]int, order []int) bool {
	if len(order) != len(issues) {
		return false
	}
	bands := make(map[int]issueBand, len(issues))
	for _, issue := range issues {
		f := factFor(issue)
		bug := 1
		if f.bug {
			bug = 0
		}
		bands[issue.Number] = issueBand{bug: bug, ver: effectiveVersion(issue, f, assign), pri: f.priority}
	}
	for j := 1; j < len(order); j++ {
		if bands[order[j]].less(bands[order[j-1]]) {
			return false
		}
	}
	pos := make(map[int]int, len(order))
	for i, n := range order {
		pos[n] = i
	}
	for _, issue := range issues {
		for _, ref := range parseIssueDeps(issue.Body) {
			if ref.target == issue.Number {
				continue
			}
			p, self := pos[issue.Number]
			q, target := pos[ref.target]
			if !self || !target {
				continue // the other issue is not in the open set
			}
			if ref.dir == "after" && q >= p {
				return false
			}
			if ref.dir == "before" && q <= p {
				return false
			}
		}
	}
	return true
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// TriagePrompt is self-contained: the model never reads repository files, so
// the triage session runs read-only from the state directory.
func TriagePrompt(issues []QueueIssue) string {
	versions := map[string]bool{}
	for _, issue := range issues {
		if v := versionFromText(issue.Milestone.Title); v != nil {
			versions[formatVersion(v)] = true
		}
	}
	var b strings.Builder
	b.WriteString("You are the task ordering engine for a repository delivery pipeline. Below is the full list of open eligible GitHub issues of one repository, with the facts already parsed from them.\n\n")
	b.WriteString("Decide the implementation order. These rules are mandatory:\n")
	b.WriteString("1. Bug issues (flag bug) always come first, before every non-bug issue.\n")
	b.WriteString("2. Milestone version, ascending: v0.1 before v0.2. Issues marked \"no milestone\" must be assigned a milestone version from their title and body: use one of the known versions when it fits, otherwise a sensible vX.Y, otherwise \"none\" (which sorts last).\n")
	b.WriteString("3. One group is (bug status, milestone version, priority label). Keep the groups in the order of rules 1-2; you may not move an issue to another group. Inside one group you may reorder freely to group related work; keep unrelated issues in creation order.\n")
	b.WriteString("4. Dependencies: an issue must come after every issue it depends on and before every issue it blocks.\n")
	if len(versions) > 0 {
		list := make([]string, 0, len(versions))
		for v := range versions {
			list = append(list, v)
		}
		sort.Slice(list, func(i, j int) bool {
			return compareVersions(versionFromText(list[i]), versionFromText(list[j])) < 0
		})
		b.WriteString("\nKnown milestone versions: " + strings.Join(list, ", ") + "\n")
	}
	b.WriteString("\nRespond with STRICT JSON ONLY, no markdown fences, no commentary:\n")
	b.WriteString("{\"assignments\":[{\"n\":<issue number>,\"milestone\":\"<version or none>\"}],\"order\":[{\"n\":<issue number>,\"why\":\"<one short sentence>\"}]}\n")
	b.WriteString("'assignments' lists only the issues marked \"no milestone\", each exactly once. 'order' lists every listed issue exactly once, in your final order.\n\nIssues:\n")
	for _, issue := range issues {
		f := factFor(issue)
		fmt.Fprintf(&b, "#%d %q (created %s", issue.Number, issue.Title, issue.CreatedAt.Format("2006-01-02"))
		if strings.TrimSpace(issue.Milestone.Title) != "" {
			fmt.Fprintf(&b, ", milestone %q", issue.Milestone.Title)
		} else {
			b.WriteString(", no milestone")
		}
		if f.priority < 3 {
			fmt.Fprintf(&b, ", priority P%d", f.priority)
		}
		if f.bug {
			b.WriteString(", bug")
		}
		if len(issue.Labels) > 0 {
			names := make([]string, 0, len(issue.Labels))
			for _, l := range issue.Labels {
				names = append(names, l.Name)
			}
			fmt.Fprintf(&b, ", labels [%s]", strings.Join(names, ","))
		}
		b.WriteString(")\n")
		for _, ref := range parseIssueDeps(issue.Body) {
			fmt.Fprintf(&b, "  dep: %s #%d\n", ref.dir, ref.target)
		}
		excerpt := oneLine(issue.Body)
		if len(excerpt) > 300 {
			excerpt = excerpt[:300] + "..."
		}
		if excerpt != "" {
			b.WriteString("  body: " + excerpt + "\n")
		}
	}
	return b.String()
}

func (a *App) storeQueueArtifact(name, text string) error {
	return atomicBytes(filepath.Join(a.cfg.StateDir, "triage", name), []byte(text))
}

// bandLabel renders the ordering band of one issue for reports.
func bandLabel(issue QueueIssue, f issueFact, ver []int, fromInference bool) string {
	var parts []string
	if f.bug {
		parts = append(parts, "bug")
	}
	switch {
	case ver != nil && fromInference:
		parts = append(parts, "inferred "+formatVersion(ver))
	case ver != nil:
		parts = append(parts, formatVersion(ver))
	default:
		parts = append(parts, "unassigned")
	}
	if f.priority < 3 {
		parts = append(parts, "P"+strconv.Itoa(f.priority))
	}
	return strings.Join(parts, "+")
}

// triageIssueRows renders one row per queue entry for the JSON artifact.
func triageIssueRows(issues []QueueIssue, plan QueuePlan) []map[string]any {
	byNumber := make(map[int]QueueIssue, len(issues))
	for _, issue := range issues {
		byNumber[issue.Number] = issue
	}
	rows := make([]map[string]any, 0, len(plan.Order))
	for pos, n := range plan.Order {
		issue, ok := byNumber[n]
		if !ok {
			continue
		}
		f := factFor(issue)
		inferred := plan.Assign[n] != nil && f.version == nil
		ver := effectiveVersion(issue, f, plan.Assign)
		why := plan.Why[n]
		if why == "" {
			why = bandLabel(issue, f, ver, inferred)
		}
		row := map[string]any{
			"pos":     pos,
			"n":       n,
			"title":   issue.Title,
			"band":    bandLabel(issue, f, ver, inferred),
			"why":     why,
			"bug":     f.bug,
			"created": issue.CreatedAt.Format("2006-01-02"),
		}
		if strings.TrimSpace(issue.Milestone.Title) != "" {
			row["milestone"] = issue.Milestone.Title
		}
		if inferred {
			row["inferred_milestone"] = formatVersion(ver)
		}
		if f.priority < 3 {
			row["priority"] = "P" + strconv.Itoa(f.priority)
		}
		rows = append(rows, row)
	}
	return rows
}

// triageReport renders the markdown queue report stored with the plan.
func triageReport(issues []QueueIssue, plan QueuePlan, source string) string {
	byNumber := make(map[int]QueueIssue, len(issues))
	for _, issue := range issues {
		byNumber[issue.Number] = issue
	}
	var b strings.Builder
	var queueLine []string
	for _, n := range plan.Order {
		queueLine = append(queueLine, "#"+strconv.Itoa(n))
	}
	b.WriteString("# Triage\n\n")
	b.WriteString(fmt.Sprintf("Issues: %d\nOrder source: %s\nQueue: %s\n\n", len(issues), source, strings.Join(queueLine, " ")))
	b.WriteString("| # | issue | band | milestone | why |\n")
	b.WriteString("|---|-------|------|-----------|-----|\n")
	for pos, n := range plan.Order {
		issue, ok := byNumber[n]
		if !ok {
			continue
		}
		f := factFor(issue)
		inferred := plan.Assign[n] != nil && f.version == nil
		ver := effectiveVersion(issue, f, plan.Assign)
		milestone := strings.TrimSpace(issue.Milestone.Title)
		switch {
		case milestone != "":
			if inferred {
				milestone += " (inferred " + formatVersion(ver) + ")"
			}
		case ver != nil:
			milestone = "inferred " + formatVersion(ver)
		default:
			milestone = "-"
		}
		why := plan.Why[n]
		if why == "" {
			why = bandLabel(issue, f, ver, inferred)
		}
		title := strings.ReplaceAll(issue.Title, "|", "\\|")
		if len(title) > 48 {
			title = title[:48] + "…"
		}
		fmt.Fprintf(&b, "| %d | #%d %s | %s | %s | %s |\n", pos+1, n, title, bandLabel(issue, f, ver, inferred), milestone, why)
	}
	return b.String()
}

// planTaskQueue builds the execution order: Go/gh collects the facts, one LLM
// call assigns missing milestones and refines the order inside bands, and
// any unusable LLM answer falls back to the deterministic rule stack. Errors
// are reserved for collection failures, which the retry machinery handles.
func (a *App) planTaskQueue(ctx context.Context, s State) ([]QueueIssue, []int, string, error) {
	issues, err := a.collectQueue(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	llmText := ""
	if len(issues) > 0 && len(issues) <= triageLLMCap {
		text, llmErr := a.runStageIn(ctx, s, "triage", TriagePrompt(issues), true, a.cfg.StateDir)
		if llmErr != nil {
			log.Printf("triage LLM call failed, using deterministic order: %s", llmErr)
		} else {
			llmText = text
		}
	}
	plan := PlanQueue(issues, llmText)
	source := "deterministic"
	if plan.FromLLM {
		source = "llm"
	}
	report := triageReport(issues, plan, source)
	stamp := time.Now().UTC().Format("20060102T150405.000000000")
	data, _ := json.Marshal(map[string]any{
		"repo": a.cfg.Repo, "milestone": a.cfg.Milestone, "label": a.cfg.Label,
		"source": source, "order": plan.Order, "issues": triageIssueRows(issues, plan),
	})
	_ = a.storeQueueArtifact("order-"+stamp+".json", string(data))
	_ = a.storeQueueArtifact("order-"+stamp+".md", report)
	return issues, plan.Order, report, nil
}

// PlanTasks runs the triage planning once and returns the readable report.
// The `triage` CLI action exposes it for auditing the order without
// executing any issue; the plan is stored as an artifact only.
func (a *App) PlanTasks(ctx context.Context) (string, error) {
	_, _, report, err := a.planTaskQueue(ctx, a.snapshot())
	return report, err
}
