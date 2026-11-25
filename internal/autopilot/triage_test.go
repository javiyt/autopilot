package autopilot

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func sampleIssues() []QueueIssue {
	d := func(day int) time.Time {
		return time.Date(2026, 11, day, 12, 0, 0, 0, time.UTC)
	}
	return []QueueIssue{
		{Number: 3, Title: "Broken link", CreatedAt: d(1), URL: "https://github.com/owner/repo/issues/3",
			Body: "The /docs link is broken. Depends on #12 for the routing layer."},
		{Number: 8, Title: "Docs page", CreatedAt: d(2), URL: "https://github.com/owner/repo/issues/8",
			Body: "Add a landing page under docs."},
		{Number: 12, Title: "Routing layer", CreatedAt: d(3), URL: "https://github.com/owner/repo/issues/12",
			Body: "Introduce the routing layer. Blocks #8."},
	}
}

func TestParseIssueDeps(t *testing.T) {
	refs := parseIssueDeps("Depends on #12 for now. Also blocks issue 8 later.")
	want := map[depRef]bool{{target: 12, dir: "after"}: true, {target: 8, dir: "before"}: true}
	if len(refs) != 2 || !want[refs[0]] || !want[refs[1]] || refs[0] == refs[1] {
		t.Fatalf("got %+v", refs)
	}
	if got := parseIssueDeps("No dependency words here, just #99 on its own."); len(got) != 0 {
		t.Fatalf("bare issue reference must not count as a dependency: %+v", got)
	}
}

func TestParseIssueDepsListForms(t *testing.T) {
	cases := []struct {
		body string
		want []depRef
	}{
		{"Dependencies: #12, #34", []depRef{{target: 12, dir: "after"}, {target: 34, dir: "after"}}},
		{"**Dependencies:** HS-006,HS-045(#64)", []depRef{{target: 64, dir: "after"}}},
		{"**Dependencies:** FEEDS-10, FEEDS-11. **Related:** #8, #22, #28, #131", nil},
		{"- Blocked by: #8, #9", []depRef{{target: 8, dir: "after"}, {target: 9, dir: "after"}}},
		{"Blocks: #8, #9", []depRef{{target: 8, dir: "before"}, {target: 9, dir: "before"}}},
		{"No dependencies: none here.", nil},
	}
	for _, c := range cases {
		got := parseIssueDeps(c.body)
		if len(got) != len(c.want) {
			t.Fatalf("%q: got %+v, want %+v", c.body, got, c.want)
		}
		for i := range c.want {
			if got[i] != c.want[i] {
				t.Fatalf("%q: got %+v, want %+v", c.body, got, c.want)
			}
		}
	}
}

func TestVersionFromText(t *testing.T) {
	cases := map[string][]int{
		"v0.1 - End-to-end MVP": {0, 1},
		"v1 - GA":               {1},
		"2.0 - Release":         {2, 0},
		"v0.10 hotfix":          {0, 10},
		"Backlog":               nil,
		"":                      nil,
	}
	for in, want := range cases {
		got := versionFromText(in)
		if (got == nil) != (want == nil) || (want != nil && compareVersions(got, want) != 0) {
			t.Fatalf("versionFromText(%q) = %v, want %v", in, got, want)
		}
	}
	// Numeric, not lexical: v0.2 sorts before v0.10.
	if compareVersions([]int{0, 2}, []int{0, 10}) >= 0 {
		t.Fatalf("v0.2 must sort before v0.10")
	}
}

func TestFactFor(t *testing.T) {
	issue := QueueIssue{
		Number:    9,
		Milestone: typedMilestone("v0.2 - Portable Architecture"),
		Labels:    typedLabels("type:bug", "priority:P1"),
	}
	f := factFor(issue)
	if !f.bug || f.priority != 1 || len(f.version) != 2 || f.version[0] != 0 || f.version[1] != 2 {
		t.Fatalf("got %+v", f)
	}
	plain := factFor(QueueIssue{Number: 10})
	if plain.bug || plain.priority != 3 || plain.version != nil {
		t.Fatalf("plain issue must be unflagged: %+v", plain)
	}
}

func typedMilestone(title string) struct {
	Title string `json:"title"`
} {
	return struct {
		Title string `json:"title"`
	}{Title: title}
}

func typedLabels(names ...string) []struct {
	Name string `json:"name"`
} {
	labels := make([]struct {
		Name string `json:"name"`
	}, len(names))
	for i, n := range names {
		labels[i] = struct {
			Name string `json:"name"`
		}{Name: n}
	}
	return labels
}

func TestDeterministicQueueMilestonesPriorityBugs(t *testing.T) {
	d := func(day int) time.Time {
		return time.Date(2026, 11, day, 12, 0, 0, 0, time.UTC)
	}
	issues := []QueueIssue{
		{Number: 27, Title: "feed quality", CreatedAt: d(1), Milestone: typedMilestone("v0.4 - Story intelligence"), Labels: typedLabels("priority:P1")},
		{Number: 110, Title: "release gate", CreatedAt: d(10), Milestone: typedMilestone("v0.1 - End-to-end MVP"), Labels: typedLabels("priority:P0")},
		{Number: 112, Title: "runbook", CreatedAt: d(11), Milestone: typedMilestone("v0.1 - End-to-end MVP"), Labels: typedLabels("priority:P1")},
		{Number: 130, Title: "ui polish", CreatedAt: d(12)},
		{Number: 200, Title: "crash on ingest", CreatedAt: d(13), Milestone: typedMilestone("v0.7 - Optional AI"), Labels: typedLabels("bug", "priority:P2")},
	}
	queue := DeterministicQueue(issues)
	want := []int{200, 110, 112, 27, 130}
	if !reflect.DeepEqual(queue, want) {
		t.Fatalf("got %v, want %v (bug first, then milestone asc, priority, unversioned last)", queue, want)
	}
}

func TestDeterministicQueueWithAssignments(t *testing.T) {
	issues := []QueueIssue{
		{Number: 110, Milestone: typedMilestone("v0.1 - End-to-end MVP")},
		{Number: 27, Milestone: typedMilestone("v0.4 - Story intelligence")},
		{Number: 130}, // no milestone: unversioned without an assignment,
		// v0.3 with one
	}
	if queue := DeterministicQueue(issues); !reflect.DeepEqual(queue, []int{110, 27, 130}) {
		t.Fatalf("without assignment got %v", queue)
	}
	queue := DeterministicQueueWithAssignments(issues, map[int][]int{130: {0, 3}})
	if !reflect.DeepEqual(queue, []int{110, 130, 27}) {
		t.Fatalf("with v0.3 assignment got %v", queue)
	}
}

func TestDeterministicQueueDependenciesBeatRank(t *testing.T) {
	issues := []QueueIssue{
		{Number: 50, Body: "Depends on #20.", Milestone: typedMilestone("v0.1 - a")},
		{Number: 20, Body: "", Milestone: typedMilestone("v0.5 - b")},
	}
	queue := DeterministicQueue(issues)
	if !reflect.DeepEqual(queue, []int{20, 50}) {
		t.Fatalf("a blocker in a higher milestone must come first, got %v", queue)
	}
}

func TestDeterministicQueueHonorsDependencies(t *testing.T) {
	// 12 comes before 8 (blocks) and before 3 (3 depends on 12); ties break by number.
	queue := DeterministicQueue(sampleIssues())
	if !reflect.DeepEqual(queue, []int{12, 3, 8}) {
		t.Fatalf("got %v, want [12 3 8]", queue)
	}
}

func TestDeterministicQueueBreaksCycles(t *testing.T) {
	issues := []QueueIssue{
		{Number: 2, Body: "Depends on #1."},
		{Number: 1, Body: "Depends on #2."},
	}
	queue := DeterministicQueue(issues)
	if len(queue) != 2 || queue[0] != 1 || queue[1] != 2 {
		t.Fatalf("cycle must be broken toward the smallest number, got %v", queue)
	}
}

func TestPlanQueueAcceptsValidLLMAnswer(t *testing.T) {
	issues := []QueueIssue{
		{Number: 5, Milestone: typedMilestone("v0.2 - x"), Labels: typedLabels("priority:P1")},
		{Number: 6, Milestone: typedMilestone("v0.2 - x"), Labels: typedLabels("priority:P0")},
		{Number: 7}, // no milestone
		{Number: 8, Milestone: typedMilestone("v0.1 - y"), Labels: typedLabels("bug")},
	}
	// Valid: bug band, then v0.1 (assigned issue 7), then the v0.2 group.
	plan := PlanQueue(issues, `{"assignments":[{"n":7,"milestone":"v0.1"}],"order":[{"n":8,"why":"bug"},{"n":7,"why":"v0.1"},{"n":6,"why":"P0"},{"n":5,"why":"P1"}]}`)
	if !plan.FromLLM || !reflect.DeepEqual(plan.Order, []int{8, 7, 6, 5}) {
		t.Fatalf("got %+v", plan)
	}
	if len(plan.Assign) != 1 || compareVersions(plan.Assign[7], []int{0, 1}) != 0 {
		t.Fatalf("assignments = %+v", plan.Assign)
	}
	// Same rules in the legacy bare-array form.
	plan = PlanQueue(sampleIssues(), "```json\n[12, 8, 3]\n```")
	if !plan.FromLLM || !reflect.DeepEqual(plan.Order, []int{12, 8, 3}) {
		t.Fatalf("bare-number fenced form: got %+v", plan)
	}
}

func TestPlanQueueRejectsBandCrossing(t *testing.T) {
	issues := []QueueIssue{
		{Number: 5, Milestone: typedMilestone("v0.2 - x"), Labels: typedLabels("priority:P1")},
		{Number: 6, Milestone: typedMilestone("v0.2 - x"), Labels: typedLabels("priority:P0")},
		{Number: 7},
		{Number: 8, Milestone: typedMilestone("v0.1 - y"), Labels: typedLabels("bug")},
	}
	// P1 before P0 inside the same milestone: a band backwards.
	plan := PlanQueue(issues, `{"assignments":[{"n":7,"milestone":"none"}],"order":[{"n":8},{"n":5},{"n":6},{"n":7}]}`)
	if plan.FromLLM {
		t.Fatalf("band crossing must be rejected: %+v", plan)
	}
	if !reflect.DeepEqual(plan.Order, []int{8, 6, 5, 7}) {
		t.Fatalf("deterministic fallback got %v", plan.Order)
	}
}

func TestPlanQueueRejectsDependencyViolation(t *testing.T) {
	issues := sampleIssues() // 3 depends on 12, 12 blocks 8: one single band
	plan := PlanQueue(issues, `[3, 8, 12]`)
	if plan.FromLLM {
		t.Fatalf("dependency violation must be rejected: %+v", plan)
	}
	if !reflect.DeepEqual(plan.Order, []int{12, 3, 8}) {
		t.Fatalf("deterministic fallback got %v", plan.Order)
	}
}

func TestPlanQueueKeepsAssignmentsWhenOrderFails(t *testing.T) {
	issues := []QueueIssue{
		{Number: 5, Milestone: typedMilestone("v0.2 - x")},
		{Number: 7}, // no milestone
		{Number: 8}, // no milestone
	}
	// The LLM assigns milestones but its order is incomplete: the
	// assignments survive into the deterministic order.
	plan := PlanQueue(issues, `{"assignments":[{"n":7,"milestone":"v0.1"},{"n":8,"milestone":"v0.3"}],"order":[{"n":5}]}`)
	if plan.FromLLM {
		t.Fatalf("incomplete order must not be accepted: %+v", plan)
	}
	if !reflect.DeepEqual(plan.Order, []int{7, 5, 8}) {
		t.Fatalf("deterministic order with assignments got %v", plan.Order)
	}
}

func TestPlanQueueRejectsInvalidLLMAnswer(t *testing.T) {
	issues := sampleIssues()
	cases := []string{
		"garbage that is not json",
		`[{"n":12}]`,                   // missing issues
		`[{"n":12},{"n":12},{"n":8}]`,  // duplicate
		`[{"n":12},{"n":8},{"n":999}]`, // unknown issue
	}
	for _, text := range cases {
		plan := PlanQueue(issues, text)
		if plan.FromLLM {
			t.Fatalf("%q must not be accepted", text)
		}
		if !reflect.DeepEqual(plan.Order, []int{12, 3, 8}) {
			t.Fatalf("%q: fallback got %v", text, plan.Order)
		}
	}
}

func TestParsePlanChecks(t *testing.T) {
	plan := "## Changes\n\n## Local verification\nReproduce CI locally.\n\n```checks\ngo test ./...\n# regenerate fixtures\ngo vet ./...\n```\n\nSummary of plan."
	checks := parsePlanChecks(plan)
	if !reflect.DeepEqual(checks, []string{"go test ./...", "go vet ./..."}) {
		t.Fatalf("got %v", checks)
	}
	if got := parsePlanChecks("no checks block at all"); got != nil {
		t.Fatalf("missing block must yield nil, got %v", got)
	}
	if got := parsePlanChecks("```checks\n\n```"); got != nil {
		t.Fatalf("empty block must yield nil, got %v", got)
	}
}

func TestMergeChecks(t *testing.T) {
	got := mergeChecks([]string{"make check", "go test ./..."}, []string{"go test ./...", "make lint"})
	want := []string{"make check", "go test ./...", "make lint"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestReadConfigTriageFallsBackToPlan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	source := `repo: owner/repo
state_dir: ./work
check_commands: ["go test ./..."]
models:
  investigate: {provider: omlx, id: reviewer, thinking: low}
  plan: {provider: omlx, id: planner, thinking: medium}
  implement: {provider: omlx, id: coder, thinking: "off"}
  review: {provider: omlx, id: reviewer, thinking: low}
  chat: {provider: omlx, id: reviewer, thinking: low}
allowed_models: [planner, coder, reviewer]
telegram_chat_id: 123
`
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ReadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Models["triage"].ID != "planner" || cfg.Models["triage"].Thinking != "medium" {
		t.Fatalf("triage must inherit the plan profile: %+v", cfg.Models["triage"])
	}
	// An explicit triage model must appear in allowed_models.
	source = `repo: owner/repo
state_dir: ./work
check_commands: ["go test ./..."]
models:
  investigate: {provider: omlx, id: reviewer, thinking: low}
  plan: {provider: omlx, id: planner, thinking: medium}
  implement: {provider: omlx, id: coder, thinking: "off"}
  review: {provider: omlx, id: reviewer, thinking: low}
  chat: {provider: omlx, id: reviewer, thinking: low}
  triage: {provider: omlx, id: sneaky, thinking: low}
allowed_models: [planner, coder, reviewer]
telegram_chat_id: 123
`
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadConfig(path); err == nil {
		t.Fatal("triage model outside allowed_models must be rejected")
	}
}

func TestReadConfigAllowsEmptyRepoForManualMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	source := `repo: ""
state_dir: ./work
check_commands: ["go test ./..."]
models:
  investigate: {provider: omlx, id: reviewer, thinking: low}
  plan: {provider: omlx, id: planner, thinking: medium}
  implement: {provider: omlx, id: coder, thinking: "off"}
  review: {provider: omlx, id: reviewer, thinking: low}
  chat: {provider: omlx, id: reviewer, thinking: low}
allowed_models: [planner, coder, reviewer]
telegram_chat_id: 123
`
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ReadConfig(path)
	if err != nil {
		t.Fatalf("empty repo must load for manual mode: %v", err)
	}
	if cfg.Repo != "" {
		t.Fatalf("repo = %q, want empty", cfg.Repo)
	}
}

func TestBeginCommandAliasAndPhase(t *testing.T) {
	for _, cmd := range []string{"/begin owner/repo milestone-x", "/start owner/repo"} {
		dir := t.TempDir()
		cfg := testConfig(dir)
		a := &App{cfg: cfg, statePath: filepath.Join(dir, "state.json"), state: DefaultState(cfg)}
		a.HandleTelegram(context.Background(), 1, cmd)
		s := a.snapshot()
		if a.cfg.Repo != "owner/repo" {
			t.Fatalf("%s: repo = %q", cmd, a.cfg.Repo)
		}
		if s.Phase != "triage" {
			t.Fatalf("%s: phase = %q, want triage", cmd, s.Phase)
		}
	}
	// A malformed repo must be rejected, not sent to gh.
	dir := t.TempDir()
	cfg := testConfig(dir)
	a := &App{cfg: cfg, statePath: filepath.Join(dir, "state.json"), state: DefaultState(cfg)}
	a.HandleTelegram(context.Background(), 1, "/begin not a valid repo with spaces")
	if a.cfg.Repo != "owner/repo" {
		t.Fatalf("invalid repo must not be applied: %q", a.cfg.Repo)
	}
}

func TestRestartRestoresRepoFromState(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	for _, tool := range []string{"git", "gh", "pi"} {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	// First run: the user began work via Telegram, so the state remembers the repo.
	cfg := testConfig(dir)
	a, err := NewApp(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.beginIssue(Issue{Number: 7, Title: "x", URL: "https://github.com/owner/repo/issues/7"}, []int{7}); err != nil {
		t.Fatal(err)
	}
	a.Close()

	// Restart with an empty configured repo: the persisted target must be restored.
	cfg2 := testConfig(dir)
	cfg2.Repo = ""
	a2, err := NewApp(cfg2, "")
	if err != nil {
		t.Fatal(err)
	}
	defer a2.Close()
	if a2.cfg.Repo != "owner/repo" {
		t.Fatalf("repo not restored after restart: %q", a2.cfg.Repo)
	}
	if a2.snapshot().Issue.Number != 7 {
		t.Fatalf("issue not restored: %+v", a2.snapshot().Issue)
	}
}

func TestEnterNoWorkNotifiesOnceState(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	a := &App{cfg: cfg, statePath: filepath.Join(dir, "state.json"), state: DefaultState(cfg)}
	a.enterNoWork()
	s := a.snapshot()
	if s.Phase != "no_work" || !s.NoWorkNotified {
		t.Fatalf("state = phase %q notified %t", s.Phase, s.NoWorkNotified)
	}
	a.enterNoWork() // idempotent
	s = a.snapshot()
	if s.Phase != "no_work" || !s.NoWorkNotified {
		t.Fatalf("second call must keep no_work: phase %q", s.Phase)
	}
	// Starting an issue clears the flag so a later empty queue notifies again.
	if err := a.beginIssue(Issue{Number: 5, Title: "x"}, []int{5}); err != nil {
		t.Fatal(err)
	}
	s = a.snapshot()
	if s.NoWorkNotified || s.Phase != "repository_setup" || len(s.Queue) != 1 {
		t.Fatalf("after beginIssue: phase %q notified %t queue %v", s.Phase, s.NoWorkNotified, s.Queue)
	}
}

func TestOpenQueueFromBaseline(t *testing.T) {
	seen := []int{3, 8, 12}
	order := []int{12, 3, 8}

	// Unchanged open set: the planned order is reused, nothing is new.
	queue, hasNew := openQueueFromBaseline(seen, order, []int{12, 3, 8})
	if hasNew || !reflect.DeepEqual(queue, []int{12, 3, 8}) {
		t.Fatalf("unchanged set: queue %v hasNew %t", queue, hasNew)
	}

	// Shrunken set (issue 3 closed): survivors keep their relative order,
	// and the closure is not treated as new work.
	queue, hasNew = openQueueFromBaseline(seen, order, []int{12, 8})
	if hasNew || !reflect.DeepEqual(queue, []int{12, 8}) {
		t.Fatalf("shrunken set: queue %v hasNew %t", queue, hasNew)
	}

	// Grown set: a number never triaged forces the full triage again.
	queue, hasNew = openQueueFromBaseline(seen, order, []int{12, 3, 8, 45})
	if !hasNew || queue != nil {
		t.Fatalf("grown set: queue %v hasNew %t", queue, hasNew)
	}

	// No baseline at all (first run): every open issue is new.
	queue, hasNew = openQueueFromBaseline(nil, nil, []int{7})
	if !hasNew || queue != nil {
		t.Fatalf("no baseline: queue %v hasNew %t", queue, hasNew)
	}

	// Baseline covers the seen set but every triaged issue closed meanwhile:
	// the filtered queue is empty, so the caller must re-plan.
	queue, hasNew = openQueueFromBaseline([]int{1}, []int{1}, nil)
	if hasNew || len(queue) != 0 {
		t.Fatalf("all closed: queue %v hasNew %t", queue, hasNew)
	}
}
