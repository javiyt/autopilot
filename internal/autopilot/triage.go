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
	depsBefore = regexp.MustCompile(`(?i)\b(?:blocks?|unblocks?|prerequisite\s+(?:for|of)|must\s+land\s+before)\s+(?:issue\s+|gh\s+)?#?(\d{1,6})\b`)
	depsAfter  = regexp.MustCompile(`(?i)\bdepends\s+on\b\s+(?:issue\s+|gh\s+)?#?(\d{1,6})\b`)
)

// parseIssueDeps extracts deterministic dependency hints from an issue body.
func parseIssueDeps(body string) []depRef {
	var refs []depRef
	seen := map[depRef]bool{}
	patterns := []struct {
		re  *regexp.Regexp
		dir string
	}{
		{depsBefore, "before"},
		{depsAfter, "after"},
	}
	for _, p := range patterns {
		for _, m := range p.re.FindAllStringSubmatch(body, -1) {
			n, err := strconv.Atoi(m[1])
			if err != nil || n <= 0 {
				continue
			}
			ref := depRef{target: n, dir: p.dir}
			if !seen[ref] {
				seen[ref] = true
				refs = append(refs, ref)
			}
		}
	}
	return refs
}

// DeterministicQueue orders issues topologically by the parsed dependencies
// and breaks ties with the issue number, which GitHub assigns in creation
// order. Cycles are broken toward the smallest number, so the function
// always returns a complete permutation of the input.
func DeterministicQueue(issues []QueueIssue) []int {
	known := map[int]bool{}
	for _, issue := range issues {
		known[issue.Number] = true
	}
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
	remaining := map[int]bool{}
	for _, issue := range issues {
		remaining[issue.Number] = true
	}
	queue := make([]int, 0, len(issues))
	for len(remaining) > 0 {
		var ready []int
		for n := range remaining {
			blocked := false
			for blocker := range before[n] {
				if remaining[blocker] {
					blocked = true
					break
				}
			}
			if !blocked {
				ready = append(ready, n)
			}
		}
		if len(ready) == 0 {
			// A dependency cycle: release the smallest number deterministically.
			for n := range remaining {
				if len(ready) == 0 || n < ready[0] {
					ready = []int{n}
				}
			}
		}
		sort.Ints(ready)
		pick := ready[0]
		queue = append(queue, pick)
		delete(remaining, pick)
	}
	return queue
}

// PlanQueue turns the collected issues into an execution order. A valid LLM
// answer must cover every issue exactly once; anything else falls back to
// the deterministic topological order.
func PlanQueue(issues []QueueIssue, llmText string) (queue []int, fromLLM bool) {
	if strings.TrimSpace(llmText) != "" {
		if order := parseLLMOrder(issues, llmText); order != nil {
			return order, true
		}
	}
	return DeterministicQueue(issues), false
}

func parseLLMOrder(issues []QueueIssue, raw string) []int {
	text := strings.TrimSpace(raw)
	if strings.HasPrefix(text, "```") {
		if nl := strings.Index(text, "\n"); nl >= 0 {
			text = text[nl+1:]
		}
		text = strings.TrimSuffix(strings.TrimSpace(text), "```")
		text = strings.TrimSpace(text)
	}
	if i := strings.Index(text, "["); i > 0 {
		text = text[i:]
	}
	if j := strings.LastIndex(text, "]"); j >= 0 {
		text = text[:j+1]
	}
	known := map[int]bool{}
	for _, issue := range issues {
		known[issue.Number] = true
	}
	var candidates [][]int
	var objs []struct {
		N int `json:"n"`
	}
	if json.Unmarshal([]byte(text), &objs) == nil {
		nums := make([]int, 0, len(objs))
		for _, o := range objs {
			nums = append(nums, o.N)
		}
		candidates = append(candidates, nums)
	}
	var nums []int
	if json.Unmarshal([]byte(text), &nums) == nil {
		candidates = append(candidates, nums)
	}
	for _, cand := range candidates {
		if len(cand) != len(issues) {
			continue
		}
		seen := map[int]bool{}
		valid := true
		for _, n := range cand {
			if !known[n] || seen[n] {
				valid = false
				break
			}
			seen[n] = true
		}
		if valid {
			return cand
		}
	}
	return nil
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// TriagePrompt is self-contained: the model never reads repository files, so
// the triage session runs read-only from the state directory.
func TriagePrompt(issues []QueueIssue) string {
	var b strings.Builder
	b.WriteString("You are the task ordering engine for a repository delivery pipeline. Below is the full list of open, eligible GitHub issues of one repository, with the dependency hints already parsed from their bodies.\n\n")
	b.WriteString("Decide the implementation order following these rules:\n")
	b.WriteString("- A prerequisite must come before the issue that depends on it.\n")
	b.WriteString("- Where no dependency exists, prefer the issue created earlier, which has the lower number.\n")
	b.WriteString("- Group tightly related issues together so the working context carries over between them.\n\n")
	b.WriteString("Respond with STRICT JSON ONLY: an array of objects {\"n\": <issue number>, \"why\": \"<one short sentence>\"} in your recommended order, including every listed issue exactly once. No markdown fences, no commentary outside the array.\n\nIssues:\n")
	for _, issue := range issues {
		fmt.Fprintf(&b, "#%d %q (created %s", issue.Number, issue.Title, issue.CreatedAt.Format("2006-01-02"))
		if issue.Milestone.Title != "" {
			fmt.Fprintf(&b, ", milestone %q", issue.Milestone.Title)
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

// planTaskQueue builds the execution order: Go/gh collects the facts, one LLM
// call decides the order, and any unusable LLM answer falls back to the
// deterministic topological order. Errors are reserved for collection
// failures, which the retry machinery handles.
func (a *App) planTaskQueue(ctx context.Context, s State) ([]QueueIssue, []int, error) {
	issues, err := a.collectQueue(ctx)
	if err != nil {
		return nil, nil, err
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
	queue, fromLLM := PlanQueue(issues, llmText)
	source := "deterministic"
	if fromLLM {
		source = "llm"
	}
	var numbers []int
	for _, issue := range issues {
		numbers = append(numbers, issue.Number)
	}
	stamp := time.Now().UTC().Format("20060102T150405.000000000")
	data, _ := json.Marshal(map[string]any{
		"repo": a.cfg.Repo, "milestone": a.cfg.Milestone, "label": a.cfg.Label,
		"source": source, "requested": numbers, "order": queue,
	})
	_ = a.storeQueueArtifact("order-"+stamp+".json", string(data))
	var queueLine []string
	for _, n := range queue {
		queueLine = append(queueLine, "#"+strconv.Itoa(n))
	}
	report := fmt.Sprintf("# Triage\n\nRepo: %s\nMilestone: %q\nLabel: %q\nOrder source: %s\nIssues: %d\nQueue: %s\n",
		a.cfg.Repo, a.cfg.Milestone, a.cfg.Label, source, len(issues), strings.Join(queueLine, " "))
	_ = a.storeQueueArtifact("order-"+stamp+".md", report)
	return issues, queue, nil
}
