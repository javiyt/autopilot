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

func TestPlanQueueAcceptsLLMOrder(t *testing.T) {
	issues := sampleIssues()
	queue, fromLLM := PlanQueue(issues, `[{"n":3,"why":"cheap fix"},{"n":8,"why":"related"},{"n":12,"why":"prereq"}]`)
	if !fromLLM || !reflect.DeepEqual(queue, []int{3, 8, 12}) {
		t.Fatalf("object form: got %v llm=%t", queue, fromLLM)
	}
	queue, fromLLM = PlanQueue(issues, "```json\n[12, 8, 3]\n```")
	if !fromLLM || !reflect.DeepEqual(queue, []int{12, 8, 3}) {
		t.Fatalf("bare-number fenced form: got %v llm=%t", queue, fromLLM)
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
		queue, fromLLM := PlanQueue(issues, text)
		if fromLLM {
			t.Fatalf("%q must not be accepted", text)
		}
		if !reflect.DeepEqual(queue, []int{12, 3, 8}) {
			t.Fatalf("%q: fallback got %v", text, queue)
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
