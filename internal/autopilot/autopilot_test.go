package autopilot

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testConfig(dir string) Config {
	return Config{
		Repo: "owner/repo", StateDir: dir, PollSeconds: 1,
		IdleTimeoutMinutes: 1, StageTimeoutMinutes: 1, MaxStageRetries: 3,
		MaxReviewRounds: 3, MaxCIFixes: 2,
		MaxLocalFixes: 3, MaxBaseSyncRounds: 3, CIWaitMinutes: 30, CheckCommands: []string{"make check"},
		Models: map[string]ModelProfile{
			"investigate": {Provider: "omlx", ID: "reviewer", Thinking: "low"},
			"plan":        {Provider: "omlx", ID: "planner", Thinking: "medium"},
			"implement":   {Provider: "omlx", ID: "coder", Thinking: "off"},
			"review":      {Provider: "omlx", ID: "reviewer", Thinking: "low"},
			"chat":        {Provider: "omlx", ID: "reviewer", Thinking: "low"},
		},
		AllowedModels:  []string{"planner", "coder", "reviewer"},
		TelegramChatID: 123,
	}
}

func TestFailureClassification(t *testing.T) {
	cases := map[string]string{
		"Compaction failed: context length exceeded": "memory/context",
		"Metal allocation returned out of memory":    "memory/context",
		"fetch failed EHOSTUNREACH":                  "connection/transient",
		"signal: killed":                             "process-killed (possible OOM)",
		"test assertions failed":                     "execution",
	}
	for input, want := range cases {
		if got := ClassifyFailure(input); got != want {
			t.Errorf("%q: got %q, want %q", input, got, want)
		}
	}
}

func TestReadConfigYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	source := `repo: owner/repo
state_dir: ./work
check_commands: ["go test ./..."]
models:
  investigate: {provider: ollama, id: reviewer, thinking: low}
  plan: {provider: ollama, id: planner, thinking: medium}
  implement: {provider: ollama, id: coder, thinking: "off"}
  review: {provider: ollama, id: reviewer, thinking: low}
  chat: {provider: ollama, id: reviewer, thinking: low}
allowed_models: [planner, coder, reviewer]
telegram_chat_id: 123
authorized_user_id: 987654321
`
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ReadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Models["plan"].ID != "planner" || cfg.Models["plan"].Thinking != "medium" {
		t.Fatalf("unexpected YAML models: %+v", cfg.Models)
	}
	if cfg.StateDir != filepath.Join(dir, "work") {
		t.Fatalf("wrong state dir: %s", cfg.StateDir)
	}
	sourceWithUnknown := source + "unknown_setting: true\n"
	if err := os.WriteFile(path, []byte(sourceWithUnknown), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadConfig(path); err == nil {
		t.Fatal("unknown YAML setting should be rejected")
	}
}

func TestCheckSummary(t *testing.T) {
	cases := []struct {
		input               string
		pass, pending, fail bool
	}{
		{`[{"name":"unit","bucket":"pass"}]`, true, false, false},
		{`[{"name":"unit","bucket":"pending"}]`, false, true, false},
		{`[{"name":"unit","bucket":"fail","state":"FAILURE"}]`, false, false, true},
		{`[{"name":"skip","bucket":"skipping"}]`, false, false, false},
		{`[{"name":"Build","bucket":"pass","state":"SUCCESS"},{"name":"Build site","bucket":"pass","state":"SUCCESS"},{"name":"Validate RSS sources","bucket":"pass","state":"SUCCESS"},{"name":"Scheduler worker","bucket":"pass","state":"SUCCESS"},{"name":"automerge","bucket":"skipping","state":"SKIPPED"}]`, true, false, false},
		{`[{"name":"unit","bucket":"pass"},{"name":"automerge","bucket":"skipping"},{"name":"lint","bucket":"pending"}]`, false, true, false},
		{`[]`, false, false, false},
	}
	for _, c := range cases {
		got, err := ParseChecks([]byte(c.input))
		if err != nil {
			t.Fatal(err)
		}
		if got.Success != c.pass || got.Pending != c.pending || got.Failed != c.fail {
			t.Errorf("%s: got %+v", c.input, got)
		}
	}
}

func TestExactDeliveryStageOrder(t *testing.T) {
	expected := []string{"repository_setup", "investigate", "plan", "implement", "local_verify", "review", "publish", "ci", "merge", "report"}
	if len(deliveryStages) != len(expected) {
		t.Fatalf("workflow stage count = %d", len(deliveryStages))
	}
	for i := range expected {
		if deliveryStages[i] != expected[i] {
			t.Errorf("stage %d: got %q, want %q", i, deliveryStages[i], expected[i])
		}
	}
	if canonicalReport("repository_setup") != "repository-setup.md" || canonicalReport("local_verify") != "local-checks.md" {
		t.Fatal("canonical artifacts should match pi-github-delivery naming")
	}
}

func TestCheckLinksForCIRecovery(t *testing.T) {
	checks, err := ParseChecks([]byte(`[{"name":"lint","bucket":"fail","state":"FAILURE","link":"https://github.com/owner/repo/actions/runs/1234/job/99"}]`))
	if err != nil || !checks.Failed || len(checks.FailedLinks) != 1 {
		t.Fatalf("missing failure evidence: %+v %v", checks, err)
	}
}

func TestSavedStateSurvivesRestartAndTelegramFix(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	a := &App{cfg: cfg, statePath: filepath.Join(dir, "state.json"), state: DefaultState(cfg)}
	if err := a.update(func(s *State) {
		s.Phase, s.BlockedPhase = "blocked", "implement"
		s.Issue = Issue{Number: 42, Title: "Test"}
		s.MergePolicy = "manual"
		s.Attempts["implement"] = 3
	}); err != nil {
		t.Fatal(err)
	}
	a.HandleTelegram(context.Background(), 123456789, "/use implement 3")
	a.HandleTelegram(context.Background(), 123456789, "/fix Try a simpler implementation")
	state, err := LoadState(a.statePath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if state.Phase != "implement" || state.RoleModels["implement"] != "reviewer" ||
		state.RecoveryPrompt != "Try a simpler implementation" || state.Attempts["implement"] != 0 {
		t.Fatalf("unexpected recovered state: %+v", state)
	}
}

func TestPiCompletionAfterIntermediateCompactionFailure(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "pi")
	script := `#!/bin/sh
printf '%s\n' '{"type":"compaction_end","errorMessage":"context length exceeded"}' '{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"VERDICT: PASS"}]} }' '{"type":"agent_settled","aborted":false}'`
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := testConfig(dir)
	res := RunPi(context.Background(), cfg, dir, "review", "omlx", "reviewer", "low", "test", filepath.Join(dir, "output.jsonl"), true)
	if res.Err != nil {
		t.Fatalf("Pi recovered internally but supervisor failed: %v", res.Err)
	}
	if strings.TrimSpace(res.Text) != "VERDICT: PASS" {
		t.Fatalf("unexpected output: %q", res.Text)
	}
}

func TestPiReportsFailureWithoutSettlement(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "pi")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\necho 'out of memory' >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := testConfig(dir)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res := RunPi(ctx, cfg, dir, "implement", "omlx", "coder", "off", "test", filepath.Join(dir, "output.jsonl"), false)
	if res.Err == nil || res.FailureKind != "memory/context" {
		t.Fatalf("expected OOM classification, got %+v", res)
	}
}

func TestPiLengthStopReasonFailsStage(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "pi")
	// A final message cut off at the output token limit settles cleanly but
	// is incomplete: the stage must fail instead of passing truncated output.
	script := `#!/bin/sh
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","stopReason":"length","content":[{"type":"text","text":"partial analysis..."}]}}' '{"type":"agent_settled","aborted":false}'`
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := testConfig(dir)
	res := RunPi(context.Background(), cfg, dir, "review", "omlx", "reviewer", "low", "test", filepath.Join(dir, "output.jsonl"), true)
	if res.Err == nil {
		t.Fatal("truncated final message must fail the stage")
	}
	if !strings.Contains(res.Err.Error(), "output token limit") {
		t.Fatalf("error must name the truncation: %v", res.Err)
	}
}

func TestPiAccumulatesFullTextAcrossTurns(t *testing.T) {
	dir := t.TempDir()
	executable := filepath.Join(dir, "pi")
	script := `#!/bin/sh
printf '%s\n' '{"type":"message_end","message":{"role":"assistant","stopReason":"toolUse","content":[{"type":"text","text":"first turn analysis"}]}}' '{"type":"message_end","message":{"role":"toolResult","content":[{"type":"text","text":"tool output"}]}}' '{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"final turn answer"}]}}' '{"type":"agent_settled","aborted":false}'`
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := testConfig(dir)
	res := RunPi(context.Background(), cfg, dir, "review", "omlx", "reviewer", "low", "test", filepath.Join(dir, "output.jsonl"), true)
	if res.Err != nil {
		t.Fatalf("session must succeed: %v", res.Err)
	}
	if res.Text != "final turn answer" {
		t.Fatalf("Text = %q, want the final assistant message", res.Text)
	}
	want := "first turn analysis\nfinal turn answer\n"
	if res.FullText != want {
		t.Fatalf("FullText = %q, want %q", res.FullText, want)
	}
}

func TestTailTruncate(t *testing.T) {
	if got := tailTruncate("short", 10); got != "short" {
		t.Fatalf("short text must pass through: %q", got)
	}
	long := strings.Repeat("a", 100)
	got := tailTruncate(long, 10)
	if !strings.HasPrefix(got, "[earlier analysis elided]\n") {
		t.Fatalf("elided head must be marked: %q", got)
	}
	if !strings.HasSuffix(got, strings.Repeat("a", 10)) {
		t.Fatalf("tail must be kept: %q", got)
	}
}

func TestHandoffRecoversTransitionAndStartsNextSessionWithArtifacts(t *testing.T) {
	dir := t.TempDir()
	checkout := filepath.Join(dir, "checkout")
	if err := os.MkdirAll(checkout, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := command(context.Background(), checkout, "git", "init", "--quiet"); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(dir)
	app := &App{cfg: cfg, checkout: checkout, statePath: filepath.Join(dir, "state.json"), state: DefaultState(cfg)}
	if err := app.update(func(s *State) {
		s.Issue = Issue{Number: 7, Title: "Feature"}
		s.Phase = "plan"
		s.MergePolicy = "manual"
	}); err != nil {
		t.Fatal(err)
	}
	state := app.snapshot()
	if err := app.recordHandoff(context.Background(), state, "plan", "implement", "Create feature in two steps"); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash between saving handoff.json and transitioning state.json.
	advanced, err := app.reconcileHandoff(app.snapshot())
	if err != nil || !advanced {
		t.Fatalf("handoff was not reconciled: %v", err)
	}
	next := app.snapshot()
	if next.Phase != "implement" {
		t.Fatalf("expected implement, got %q", next.Phase)
	}
	if prompt := app.handoffContext(next); !strings.Contains(prompt, "Create feature in two steps") {
		t.Fatalf("next session did not receive prior artifact: %s", prompt)
	}
	handoff, err := app.latestHandoff(7)
	if err != nil || handoff.MergePolicy != "manual" || !strings.HasPrefix(handoff.ReportFile, "attempts/plan/") {
		t.Fatalf("missing immutable stage evidence or merge policy: %+v %v", handoff, err)
	}
	if _, err := os.Stat(app.artifact(7, handoff.ReportFile)); err != nil {
		t.Fatal(err)
	}
}

func TestFingerprintCoversTrackedAndUntrackedChanges(t *testing.T) {
	dir := t.TempDir()
	if _, err := command(context.Background(), dir, "git", "init", "--quiet"); err != nil {
		t.Fatal(err)
	}
	tracked := filepath.Join(dir, "tracked.txt")
	if err := os.WriteFile(tracked, []byte("base"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "tracked.txt"}, {"-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", "initial"}, {"update-ref", "refs/remotes/origin/main", "HEAD"}} {
		if out, err := command(context.Background(), dir, "git", args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	cfg := testConfig(t.TempDir())
	a := &App{cfg: cfg, checkout: dir, statePath: filepath.Join(cfg.StateDir, "state.json"), state: DefaultState(cfg)}
	a.state.BaseBranch = "main"
	initial, err := a.diffFingerprint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("untracked"), 0600); err != nil {
		t.Fatal(err)
	}
	added, err := a.diffFingerprint(context.Background())
	if err != nil || added == initial {
		t.Fatalf("untracked file not detected: %v", err)
	}
	if err := os.WriteFile(tracked, []byte("modified"), 0600); err != nil {
		t.Fatal(err)
	}
	modified, err := a.diffFingerprint(context.Background())
	if err != nil || added == modified {
		t.Fatalf("tracked change not detected: %v", err)
	}
}

func TestMergeAuthorityMustBeExplicitAndImmutable(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.AutoMerge = true // YAML changed after an issue started as manual.
	a := &App{cfg: cfg, state: DefaultState(cfg)}
	a.state.MergePolicy = "manual"
	if err := a.merge(context.Background(), 123); err == nil {
		t.Fatal("manual issue must not merge even with current YAML auto_merge=true")
	}
}

func TestBaseSyncDetectsBehindHeadRebasesAndAbortsConflicts(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	git := func(d string, args ...string) {
		t.Helper()
		if out, err := command(ctx, d, "git", args...); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	commit := func(d, message, file, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(d, file), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		git(d, "add", "--all")
		git(d, "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "-m", message)
	}
	// fixture builds a bare origin plus a work clone where main moves ahead of
	// the published feature head. conflictMain=true makes main rewrite the same
	// content the feature branch changed, so the rebase conflicts.
	fixture := func(root string, conflictMain bool) (work string, featureBefore string) {
		t.Helper()
		git(root, "init", "--quiet", "--bare", filepath.Join(root, "origin.git"))
		work = filepath.Join(root, "work")
		if err := os.MkdirAll(work, 0700); err != nil {
			t.Fatal(err)
		}
		git(work, "init", "--quiet", "-b", "main")
		git(work, "remote", "add", "origin", filepath.Join(root, "origin.git"))
		commit(work, "initial", "file.txt", "one\ntwo\nthree\nfour\nfive\nsix\n")
		git(work, "push", "origin", "main")
		git(work, "switch", "-c", branchFor(160))
		commit(work, "feature change", "file.txt", "one\nfeature two\nthree\nfour\nfive\nsix\n")
		git(work, "push", "origin", branchFor(160))
		out, err := command(ctx, work, "git", "rev-parse", "HEAD")
		if err != nil {
			t.Fatal(err)
		}
		featureBefore = strings.TrimSpace(out)
		git(work, "switch", "main")
		if conflictMain {
			commit(work, "main move", "file.txt", "one\nmain two\nthree\nfour\nfive\nsix\n")
		} else {
			commit(work, "main move", "file.txt", "one\ntwo\nthree\nfour\nfive\nsix\nmain seven\n")
		}
		git(work, "push", "origin", "main")
		return work, featureBefore
	}

	// Conflict path: main rewrote the line the feature branch changed. The
	// deterministic rebase must abort and name the file.
	conflictRoot := filepath.Join(dir, "conflict")
	if err := os.MkdirAll(conflictRoot, 0700); err != nil {
		t.Fatal(err)
	}
	work, featureBefore := fixture(conflictRoot, true)
	cfg := testConfig(t.TempDir())
	a := &App{cfg: cfg, checkout: work, statePath: filepath.Join(cfg.StateDir, "state.json"), state: DefaultState(cfg)}
	a.state.BaseBranch = "main"

	if behind, err := a.baseAheadCount(ctx, featureBefore); err != nil || behind != 1 {
		t.Fatalf("expected PR head 1 commit behind moved base, got %d err=%v", behind, err)
	}
	if behind, err := a.baseAheadCount(ctx, "HEAD"); err != nil || behind != 0 {
		t.Fatalf("main HEAD must not be behind main, got %d err=%v", behind, err)
	}
	git(work, "switch", branchFor(160))
	if _, err := a.rebaseIssueBranch(ctx); err == nil {
		t.Fatal("rebase with a content conflict must fail")
	} else if !strings.Contains(err.Error(), "conflicting files: file.txt") {
		t.Fatalf("conflict evidence missing file name: %v", err)
	}
	status, _ := command(ctx, work, "git", "status", "--porcelain")
	headAfterConflict, _ := command(ctx, work, "git", "rev-parse", "HEAD")
	if strings.TrimSpace(status) != "" || strings.TrimSpace(headAfterConflict) != featureBefore {
		t.Fatalf("aborted rebase must restore the pre-rebase branch: status=%q head=%s", status, headAfterConflict)
	}

	// Clean path: main moved a different region. The rebase succeeds, the
	// lease-protected force push updates the rewritten remote branch, and
	// prepareIssueBranch rebases a clean behind branch automatically.
	cleanRoot := filepath.Join(dir, "clean")
	if err := os.MkdirAll(cleanRoot, 0700); err != nil {
		t.Fatal(err)
	}
	cleanWork, _ := fixture(cleanRoot, false)
	a.checkout = cleanWork
	a.state.BaseBranch = "main"
	if _, err := command(ctx, cleanWork, "git", "switch", branchFor(160)); err != nil {
		t.Fatal(err)
	}
	note, err := a.rebaseIssueBranch(ctx)
	if err != nil {
		t.Fatalf("clean rebase failed: %v", err)
	}
	if !strings.Contains(note, "rebased") {
		t.Fatalf("rebase note missing: %q", note)
	}
	if behind, err := a.baseAheadCount(ctx, "HEAD"); err != nil || behind != 0 {
		t.Fatalf("rebased HEAD must contain the moved base, got %d err=%v", behind, err)
	}
	if err := a.pushIssueBranch(ctx, branchFor(160)); err != nil {
		t.Fatalf("force-with-lease push of the rebased branch failed: %v", err)
	}
	remoteHead, err := command(ctx, cleanWork, "git", "rev-parse", "origin/"+branchFor(160))
	if err != nil {
		t.Fatal(err)
	}
	localHead, err := command(ctx, cleanWork, "git", "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(remoteHead) != strings.TrimSpace(localHead) {
		t.Fatalf("remote feature not updated by force push: %s vs %s", remoteHead, localHead)
	}
	git(cleanWork, "switch", "main")
	commit(cleanWork, "main moves again", "other.txt", "other\n")
	git(cleanWork, "push", "origin", "main")
	git(cleanWork, "switch", branchFor(160))
	if err := a.prepareIssueBranch(ctx, Issue{Number: 160}); err != nil {
		t.Fatalf("prepareIssueBranch must rebase a clean behind branch: %v", err)
	}
	if behind, err := a.baseAheadCount(ctx, "HEAD"); err != nil || behind != 0 {
		t.Fatalf("prepareIssueBranch left the branch behind: %d err=%v", behind, err)
	}
}

func TestInvalidHandoffCannotOverrideMergePolicy(t *testing.T) {
	cfg := testConfig(t.TempDir())
	a := &App{cfg: cfg, state: DefaultState(cfg), statePath: filepath.Join(cfg.StateDir, "state.json")}
	if err := a.update(func(s *State) { s.Issue = Issue{Number: 99}; s.Phase = "review"; s.MergePolicy = "manual" }); err != nil {
		t.Fatal(err)
	}
	h := Handoff{IssueNumber: 99, MergePolicy: "auto", FromPhase: "review", ToPhase: "publish", ReportFile: "review.md", StatusFile: "status.txt"}
	if err := a.storeArtifact(99, "review.md", "approved"); err != nil {
		t.Fatal(err)
	}
	if err := atomicJSON(a.artifact(99, "handoff.json"), h); err != nil {
		t.Fatal(err)
	}
	if advanced, err := a.reconcileHandoff(a.snapshot()); err == nil || advanced {
		t.Fatalf("policy escalation not rejected: advanced=%t err=%v", advanced, err)
	}
}

// TestRunStageInRemotePreflightNoFallback verifies issue #5: when the role's
// remote endpoint is down, runStageIn fails with an actionable error and
// never launches Pi (no silent fallback to a local provider).
func TestRunStageInRemotePreflightNoFallback(t *testing.T) {
	dir := t.TempDir()
	// A fake pi that records it was invoked. If the pre-flight works, this
	// file must never be created.
	dumpDir := filepath.Join(dir, "dump")
	if err := os.MkdirAll(dumpDir, 0700); err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(dir, "pi")
	script := `#!/bin/sh
touch "$AUTOPILOT_TEST_PI_INVOKED"
printf '%s\n' '{"type":"agent_settled","aborted":false}' '{"type":"message_end","message":{"role":"assistant","stopReason":"stop","content":[{"type":"text","text":"ok"}]}}'
`
	if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("AUTOPILOT_TEST_PI_INVOKED", filepath.Join(dumpDir, "invoked"))

	// Bind then close a server to get a real dead port on localhost.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	stateDir := filepath.Join(dir, "state")
	cfg := testConfig(stateDir)
	// The implement role targets a remote omlx endpoint that is down.
	cfg.Models["implement"] = ModelProfile{Provider: "omlx", BaseURL: deadURL + "/v1", ID: "coder", Thinking: "off"}
	a := &App{cfg: cfg, checkout: dir, statePath: filepath.Join(stateDir, "state.json"), state: DefaultState(cfg)}

	_, err := a.runStageIn(context.Background(), a.snapshot(), "implement", "test prompt", false, dir)
	if err == nil {
		t.Fatal("runStageIn must fail when the remote endpoint is unreachable")
	}
	if !strings.Contains(err.Error(), "not falling back to a local provider") {
		t.Fatalf("error must be actionable and explicit about no fallback, got: %v", err)
	}
	// The fake pi must never have been invoked.
	if _, err := os.Stat(filepath.Join(dumpDir, "invoked")); !os.IsNotExist(err) {
		t.Fatal("Pi must not be launched when the remote endpoint is down")
	}
}
