package autopilot

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// flakyModelsServer serves an OpenAI-compatible /v1/models catalog on a fixed
// local port that the test can flip up/down: down = accepted connections are
// closed immediately (the probe sees a reset, i.e. unreachable); up = a plain
// 200 with the catalog is written back. probes counts every request served.
func flakyModelsServer(t *testing.T) (url string, set func(up bool), probes *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url = "http://" + ln.Addr().String() + "/v1"
	var accepting bool
	var mu sync.Mutex
	count := &atomic.Int64{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			up := accepting
			mu.Unlock()
			go func(conn net.Conn) {
				defer conn.Close()
				if !up {
					return
				}
				_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
				buf := make([]byte, 4096)
				if n, _ := conn.Read(buf); n == 0 {
					return
				}
				count.Add(1)
				body := `{"data":[{"id":"coder"},{"id":"planner"},{"id":"reviewer"}]}`
				fmt.Fprintf(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
			}(c)
		}
	}()
	set = func(up bool) {
		mu.Lock()
		accepting = up
		mu.Unlock()
	}
	t.Cleanup(func() { ln.Close() })
	return url, set, count
}

// shrinkOutageTiming replaces the production outage/retry delays with
// sub-second values for a test and restores them afterwards.
func shrinkOutageTiming(t *testing.T, base, retry time.Duration) {
	t.Helper()
	oldOutage, oldOOM, oldRetry := outageBackoffBase, oomBackoffBase, stageRetryDelay
	outageBackoffBase, oomBackoffBase, stageRetryDelay = base, base, retry
	t.Cleanup(func() {
		outageBackoffBase, oomBackoffBase, stageRetryDelay = oldOutage, oldOOM, oldRetry
	})
}

// AC-1: persisted outage state survives a write/load round trip, and a state
// file written by an older binary (without the new fields) loads as zero
// values without a migration.
func TestOutageStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	path := filepath.Join(dir, "state.json")
	since := time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	st := DefaultState(cfg)
	st.Issue = Issue{Number: 6, Title: "outage"}
	st.Phase = "implement"
	st.MergePolicy = "auto"
	st.Attempts["implement"] = 2
	st.RemoteOutageSince = since
	st.RemoteHost = "mac-studio:8000"
	st.RemoteProbeCount = 4
	st.RemoteBackoffUntil = since.Add(3 * time.Minute)
	st.RemoteNotified = true
	st.RemoteReminderAt = since.Add(time.Hour)
	st.OOMCount = 2
	if err := atomicJSON(path, st); err != nil {
		t.Fatal(err)
	}
	got, err := LoadState(path, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got.RemoteOutageSince != since || got.RemoteHost != "mac-studio:8000" || got.RemoteProbeCount != 4 ||
		got.RemoteBackoffUntil != since.Add(3*time.Minute) || !got.RemoteNotified ||
		got.RemoteReminderAt != since.Add(time.Hour) || got.OOMCount != 2 || got.Attempts["implement"] != 2 {
		t.Fatalf("outage state did not survive the round trip: %+v", got)
	}
	// A state file from a pre-issue-#6 binary: no outage fields at all.
	old := `{"issue":{"number":5},"phase":"plan","merge_policy":"manual","attempts":{"plan":1},"role_models":{},"model_overrides_v2":true,"updated_at":"2026-11-27T09:00:00Z"}`
	oldPath := filepath.Join(dir, "old-state.json")
	if err := os.WriteFile(oldPath, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	gotOld, err := LoadState(oldPath, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !gotOld.RemoteOutageSince.IsZero() || gotOld.RemoteHost != "" || gotOld.RemoteProbeCount != 0 ||
		!gotOld.RemoteBackoffUntil.IsZero() || gotOld.RemoteNotified ||
		!gotOld.RemoteReminderAt.IsZero() || gotOld.OOMCount != 0 {
		t.Fatalf("old state must load with zero outage values: %+v", gotOld)
	}
	if gotOld.Attempts["plan"] != 1 {
		t.Fatalf("existing fields must still load: %+v", gotOld)
	}
}

// AC-3/AC-6: the failure taxonomy maps every stage-failure shape to the
// right class; preflight status wins over the error text.
func TestClassifyStageError(t *testing.T) {
	runPiErr := func(kind string) error {
		return fmt.Errorf("Pi process failed: exit status 1 (%s): provider stderr text", kind)
	}
	cases := []struct {
		name      string
		err       error
		preflight *RemoteCheckResult
		want      FailureClass
	}{
		{"preflight unreachable", errors.New("remote endpoint down"), &RemoteCheckResult{Status: StatusUnreachable}, FailureRemoteOutage},
		{"preflight auth failed", errors.New("credential rejected"), &RemoteCheckResult{Status: StatusAuthFailed}, FailureExecution},
		{"preflight model not found is not an outage", errors.New("catalog miss"), &RemoteCheckResult{Status: StatusModelNotFound}, FailureExecution},
		{"in-flight connection transient", runPiErr("connection/transient"), nil, FailureRemoteOutage},
		{"in-flight memory context", runPiErr("memory/context"), nil, FailureOOM},
		{"in-flight process killed OOM", runPiErr("process-killed (possible OOM)"), nil, FailureOOM},
		{"loop detected", errors.New("repetition loop detected: the assistant repeated a fixed sequence of turns"), nil, FailureExecution},
		{"idle timeout", errors.New("Pi did not emit output within the idle timeout"), nil, FailureExecution},
		{"stopReason error", errors.New("Pi final assistant message reported a failure"), nil, FailureExecution},
		{"gate error", errors.New("review/fix pass limit reached"), nil, FailureExecution},
		{"no final text", errors.New("Pi returned no final assistant text"), nil, FailureExecution},
		{"raw connection refused text", errors.New("dial tcp 192.168.1.5:8000: connect: connection refused"), nil, FailureRemoteOutage},
		{"plain unknown error", errors.New("something exploded"), nil, FailureExecution},
		{"nil error", nil, nil, FailureExecution},
	}
	for _, c := range cases {
		if got := ClassifyStageError(c.err, c.preflight); got != c.want {
			t.Errorf("%s: ClassifyStageError = %q, want %q", c.name, got, c.want)
		}
	}
	// stageError must surface the preflight result through the error chain.
	se := &stageError{err: errors.New("unreachable"), preflight: &RemoteCheckResult{Status: StatusUnreachable}}
	if p := preflightOf(se); p == nil || p.Status != StatusUnreachable {
		t.Fatalf("preflightOf must extract the probe result: %+v", p)
	}
	if p := preflightOf(errors.New("plain")); p != nil {
		t.Fatalf("preflightOf of a plain error must be nil: %+v", p)
	}
	if got := ClassifyStageError(se, preflightOf(se)); got != FailureRemoteOutage {
		t.Fatalf("wrapped preflight failure must classify as outage: %q", got)
	}
	if se.Error() != "unreachable" {
		t.Fatalf("stageError.Error must keep the inner text: %q", se.Error())
	}
	var inner error = errors.New("inner")
	if !errors.Is(&stageError{err: inner, preflight: nil}, inner) {
		t.Fatal("stageError must unwrap to the inner error")
	}
}

// AC-4: the backoff sequences double from their documented floors and cap at
// the documented maxima no matter how long the outage lasts.
func TestBackoffBounded(t *testing.T) {
	wantOutage := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 32 * time.Minute, 60 * time.Minute}
	for i, w := range wantOutage {
		if got := nextBackoff(i + 1); got != w {
			t.Errorf("nextBackoff(%d) = %v, want %v", i+1, got, w)
		}
	}
	for c := len(wantOutage) + 1; c <= 100; c++ {
		if got := nextBackoff(c); got != 60*time.Minute {
			t.Errorf("nextBackoff(%d) = %v, want the 60m cap", c, got)
		}
	}
	if got := nextBackoff(0); got != 30*time.Second {
		t.Errorf("nextBackoff(0) = %v, want the 30s floor", got)
	}
	wantOOM := []time.Duration{2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 16 * time.Minute, 30 * time.Minute}
	for i, w := range wantOOM {
		if got := oomBackoff(i + 1); got != w {
			t.Errorf("oomBackoff(%d) = %v, want %v", i+1, got, w)
		}
	}
	for n := len(wantOOM) + 1; n <= 50; n++ {
		if got := oomBackoff(n); got != 30*time.Minute {
			t.Errorf("oomBackoff(%d) = %v, want the 30m cap", n, got)
		}
	}
}

// AC-5: the pure decision logic emits exactly one offline message per
// episode, one online message per recovery, and at most one reminder per
// hour of simulated outage time; repeated probes emit nothing extra.
func TestOutageNotificationDedup(t *testing.T) {
	host, phase := "mac-studio:8000", "implement"
	base := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	state := State{}
	var messages []string
	round := func(healthy bool, probeCount int, now time.Time) {
		d := decideOutage(host, phase, state, healthy, probeCount, now)
		if d.NotifyOffline {
			messages = append(messages, "offline")
		}
		if d.NotifyOnline {
			messages = append(messages, "online")
		}
		if d.NotifyReminder {
			messages = append(messages, "reminder")
		}
		if d.ResetOutage {
			state = State{}
			return
		}
		state = State{
			RemoteOutageSince:  d.OutageSince,
			RemoteProbeCount:   d.ProbeCount,
			RemoteBackoffUntil: d.BackoffUntil,
			RemoteNotified:     d.Notified,
			RemoteReminderAt:   d.ReminderAt,
		}
	}
	// Episode 1: probes every 5 minutes for 150 minutes (the first hour fires
	// one reminder at t=60m, the second at t=120m; nothing in between).
	now := base
	for i := 1; i <= 30; i++ {
		now = now.Add(5 * time.Minute)
		round(false, i, now)
	}
	// Recovery.
	now = now.Add(5 * time.Minute)
	round(true, 31, now)
	if !state.RemoteOutageSince.IsZero() || state.RemoteProbeCount != 0 || state.RemoteNotified {
		t.Fatalf("recovery must clear the episode: %+v", state)
	}
	// Episode 2: one probe, then recovery.
	now = now.Add(time.Hour)
	round(false, 1, now)
	now = now.Add(30 * time.Minute)
	round(true, 2, now)
	// A healthy probe with no active episode owes nothing.
	round(true, 1, now.Add(time.Hour))
	want := []string{"offline", "reminder", "reminder", "online", "offline", "online"}
	if !reflect.DeepEqual(messages, want) {
		t.Fatalf("messages = %v, want %v", messages, want)
	}
}

// AC-2/AC-4: a full outage episode driven through handleStepFailure with a
// real (flaky) endpoint: the offline transition is persisted, the stage retry
// counters stay untouched, the probe loop respects the backoff, recovery
// resets the episode and returns so the Work loop can resume the stage in a
// fresh session.
func TestOutageProbeAndResume(t *testing.T) {
	url, set, probes := flakyModelsServer(t)
	set(false)
	shrinkOutageTiming(t, 20*time.Millisecond, 10*time.Millisecond)

	dir := t.TempDir()
	cfg := testConfig(dir)
	profile := ModelProfile{Provider: "openai-compatible", BaseURL: url, ID: "coder", Thinking: "off"}
	for role := range cfg.Models {
		cfg.Models[role] = profile
	}
	cfg.AllowedModels = []string{"coder", "planner", "reviewer"}
	a := &App{cfg: cfg, checkout: dir, statePath: filepath.Join(dir, "state.json"), state: DefaultState(cfg)}
	if err := a.update(func(s *State) {
		s.Issue = Issue{Number: 6, Title: "outage"}
		s.Phase = "plan"
		s.MergePolicy = "manual"
	}); err != nil {
		t.Fatal(err)
	}

	// The endpoint flips back up while the wait loop is probing.
	flip := make(chan struct{})
	go func() {
		time.Sleep(80 * time.Millisecond)
		set(true)
		close(flip)
	}()
	preflight := &RemoteCheckResult{ProviderKey: "openai-compatible", Status: StatusUnreachable, Message: "unreachable"}
	err := &stageError{err: errors.New("plan stage: remote endpoint unreachable — not falling back to a local provider"), preflight: preflight}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := a.handleStepFailure(ctx, "plan", err); err != nil {
		t.Fatalf("handleStepFailure must return nil once the endpoint recovers: %v", err)
	}
	<-flip
	s := a.snapshot()
	if s.Attempts["plan"] != 0 {
		t.Fatalf("outage must not burn stage retries: Attempts[plan]=%d", s.Attempts["plan"])
	}
	if s.Phase != "plan" || s.BlockedPhase != "" {
		t.Fatalf("the interrupted stage must stay pending for the fresh session: phase=%q blocked=%q", s.Phase, s.BlockedPhase)
	}
	if !s.RemoteOutageSince.IsZero() || s.RemoteHost != "" || s.RemoteProbeCount != 0 ||
		!s.RemoteBackoffUntil.IsZero() || s.RemoteNotified || !s.RemoteReminderAt.IsZero() {
		t.Fatalf("recovery must reset every persisted outage field: %+v", s)
	}
	if probes.Load() == 0 {
		t.Fatal("the recovery loop must have probed the endpoint")
	}
	// state.json must still load cleanly (AC-1).
	if _, err := LoadState(a.statePath, cfg); err != nil {
		t.Fatalf("state.json unreadable after the outage: %v", err)
	}
}

// AC-2/AC-4 regression: the endpoint may already be healthy when the first
// recovery probe runs (the server returned within seconds of an in-flight
// drop). The wait loop must terminate on that first healthy probe instead of
// spinning forever on an episode that was never persisted, and it must not
// notify or persist anything.
func TestWaitOutageEndpointAlreadyHealthy(t *testing.T) {
	url, set, _ := flakyModelsServer(t)
	set(true) // endpoint is up when the transient failure is handed over
	shrinkOutageTiming(t, 20*time.Millisecond, 10*time.Millisecond)

	dir := t.TempDir()
	cfg := testConfig(dir)
	profile := ModelProfile{Provider: "openai-compatible", BaseURL: url, ID: "coder", Thinking: "off"}
	cfg.Models["plan"] = profile
	a := &App{cfg: cfg, checkout: dir, statePath: filepath.Join(dir, "state.json"), state: DefaultState(cfg)}
	if err := a.update(func(s *State) {
		s.Issue = Issue{Number: 6, Title: "outage"}
		s.Phase = "plan"
		s.MergePolicy = "manual"
	}); err != nil {
		t.Fatal(err)
	}
	// Bounded ctx: the pre-fix hot loop would only exit on ctx expiry.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := a.handleStepFailure(ctx, "plan", errors.New("remote endpoint down (connection/transient)")); err != nil {
		t.Fatalf("a first probe that already finds the endpoint healthy must return nil: %v", err)
	}
	s := a.snapshot()
	if s.Attempts["plan"] != 0 || s.Phase == "blocked" {
		t.Fatalf("already-healthy recovery must not burn retries: Attempts=%d phase=%q", s.Attempts["plan"], s.Phase)
	}
	if !s.RemoteOutageSince.IsZero() || s.RemoteHost != "" || s.RemoteProbeCount != 0 ||
		!s.RemoteBackoffUntil.IsZero() || s.RemoteNotified || !s.RemoteReminderAt.IsZero() {
		t.Fatalf("no outage episode may be persisted when the endpoint never went down: %+v", s)
	}
}

// AC-3: repeated transient failures never exhaust the code-fix retry
// counter; a following execution failure does burn it and blocks after
// MaxStageRetries, exactly like today.
func TestTransientFailureDoesNotBurnStageRetries(t *testing.T) {
	url, set, _ := flakyModelsServer(t)
	set(false)
	shrinkOutageTiming(t, 20*time.Millisecond, 10*time.Millisecond)

	dir := t.TempDir()
	cfg := testConfig(dir)
	profile := ModelProfile{Provider: "openai-compatible", BaseURL: url, ID: "coder", Thinking: "off"}
	cfg.Models["plan"] = profile
	a := &App{cfg: cfg, checkout: dir, statePath: filepath.Join(dir, "state.json"), state: DefaultState(cfg)}
	if err := a.update(func(s *State) {
		s.Issue = Issue{Number: 6, Title: "outage"}
		s.Phase = "plan"
		s.MergePolicy = "manual"
	}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	transient := errors.New("plan stage: remote endpoint down (connection/transient)")
	for i := 0; i < 7; i++ {
		set(false)
		flip := make(chan struct{})
		go func() {
			time.Sleep(30 * time.Millisecond)
			set(true)
			close(flip)
		}()
		if err := a.handleStepFailure(ctx, "plan", transient); err != nil {
			t.Fatalf("transient failure %d must recover: %v", i+1, err)
		}
		<-flip
		s := a.snapshot()
		if s.Attempts["plan"] != 0 || s.Phase == "blocked" {
			t.Fatalf("transient failure %d burned stage retries: Attempts=%d phase=%q", i+1, s.Attempts["plan"], s.Phase)
		}
	}
	// Execution failures keep the existing retry→block behavior.
	execution := errors.New("test assertions failed")
	for i := 0; i < cfg.MaxStageRetries; i++ {
		if err := a.handleStepFailure(ctx, "plan", execution); err != nil {
			t.Fatal(err)
		}
	}
	s := a.snapshot()
	if s.Attempts["plan"] != cfg.MaxStageRetries {
		t.Fatalf("execution failures must burn the stage retry counter: Attempts=%d, want %d", s.Attempts["plan"], cfg.MaxStageRetries)
	}
	if s.Phase != "blocked" || s.BlockedPhase != "plan" {
		t.Fatalf("after MaxStageRetries execution failures the stage must block: phase=%q blocked=%q", s.Phase, s.BlockedPhase)
	}
	if s.OOMCount != 0 {
		t.Fatalf("execution failures must not touch the OOM counter: %d", s.OOMCount)
	}
}

// AC-6: OOM failures are bounded in the separate counter with a distinct
// block; they never consume stage retries, and /retry resets the OOM
// counter so a following transient outage waits unblocked.
func TestOOMBoundedSeparateCounter(t *testing.T) {
	url, set, _ := flakyModelsServer(t)
	set(false)
	shrinkOutageTiming(t, 20*time.Millisecond, 10*time.Millisecond)

	dir := t.TempDir()
	cfg := testConfig(dir)
	profile := ModelProfile{Provider: "openai-compatible", BaseURL: url, ID: "coder", Thinking: "off"}
	cfg.Models["plan"] = profile
	a := &App{cfg: cfg, checkout: dir, statePath: filepath.Join(dir, "state.json"), state: DefaultState(cfg)}
	if err := a.update(func(s *State) {
		s.Issue = Issue{Number: 6, Title: "oom"}
		s.Phase = "plan"
		s.MergePolicy = "manual"
	}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	oomErr := errors.New("Pi process failed: exit status 1 (memory/context): Metal allocation returned out of memory")
	for i := 1; i < maxOOMRetries; i++ {
		if err := a.handleStepFailure(ctx, "plan", oomErr); err != nil {
			t.Fatal(err)
		}
		s := a.snapshot()
		if s.OOMCount != i || s.Attempts["plan"] != 0 || s.Phase == "blocked" {
			t.Fatalf("after %d OOM failures: OOMCount=%d Attempts=%d phase=%q", i, s.OOMCount, s.Attempts["plan"], s.Phase)
		}
	}
	// The maxOOMRetries-th OOM failure blocks with the OOM-specific state.
	if err := a.handleStepFailure(ctx, "plan", oomErr); err != nil {
		t.Fatal(err)
	}
	s := a.snapshot()
	if s.OOMCount != maxOOMRetries || s.Phase != "blocked" || s.BlockedPhase != "plan" || s.Attempts["plan"] != 0 {
		t.Fatalf("OOM exhaustion must block without burning stage retries: %+v", s)
	}
	// /retry releases the block and resets the separate counter.
	a.HandleTelegram(ctx, 123, "/retry")
	s = a.snapshot()
	if s.Phase != "plan" || s.OOMCount != 0 || s.Attempts["plan"] != 0 {
		t.Fatalf("/retry must reset the OOM counter and restore the phase: %+v", s)
	}
	// A following transient outage waits and recovers, unblocked.
	set(false)
	flip := make(chan struct{})
	go func() {
		time.Sleep(30 * time.Millisecond)
		set(true)
		close(flip)
	}()
	if err := a.handleStepFailure(ctx, "plan", errors.New("remote endpoint down (connection/transient)")); err != nil {
		t.Fatalf("post-retry transient outage must recover: %v", err)
	}
	<-flip
	s = a.snapshot()
	if s.Phase != "plan" || s.BlockedPhase != "" || s.Attempts["plan"] != 0 || s.OOMCount != 0 {
		t.Fatalf("transient outage after /retry must not block or burn counters: %+v", s)
	}
}

// AC-4 (restart semantics): a supervisor restarted mid-outage must respect
// the persisted backoff deadline — no probe before it, no backoff reset —
// and resume when the endpoint returns.
func TestRestartDuringOutageKeepsBackoffDeadline(t *testing.T) {
	url, set, probes := flakyModelsServer(t)
	set(false)
	shrinkOutageTiming(t, 20*time.Millisecond, 10*time.Millisecond)

	dir := t.TempDir()
	cfg := testConfig(dir)
	profile := ModelProfile{Provider: "openai-compatible", BaseURL: url, ID: "coder", Thinking: "off"}
	for role := range cfg.Models {
		cfg.Models[role] = profile
	}
	cfg.AllowedModels = []string{"coder", "planner", "reviewer"}

	// The first (crashed) supervisor persisted an active episode whose
	// backoff deadline is in the future.
	first := &App{cfg: cfg, checkout: dir, statePath: filepath.Join(dir, "state.json"), state: DefaultState(cfg)}
	since := time.Now().UTC().Add(-time.Minute)
	if err := first.update(func(s *State) {
		s.Issue = Issue{Number: 6, Title: "outage"}
		s.Phase = "plan"
		s.MergePolicy = "manual"
		s.RemoteOutageSince = since
		s.RemoteHost = "127.0.0.1:0"
		s.RemoteProbeCount = 3
		s.RemoteBackoffUntil = time.Now().UTC().Add(800 * time.Millisecond)
		s.RemoteNotified = true
	}); err != nil {
		t.Fatal(err)
	}
	// Restart: a fresh App over the same state dir.
	second := &App{cfg: cfg, checkout: dir, statePath: first.statePath, state: DefaultState(cfg)}
	if s, err := LoadState(first.statePath, cfg); err != nil {
		t.Fatal(err)
	} else {
		second.state = s
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- second.handleStepFailure(ctx, "plan", errors.New("remote endpoint down (connection/transient)"))
	}()
	// Before the persisted deadline no probe may have run (backoff kept,
	// not reset by the restart).
	time.Sleep(400 * time.Millisecond)
	if n := probes.Load(); n != 0 {
		t.Fatalf("probe before the persisted backoff deadline: %d", n)
	}
	set(true)
	if err := <-done; err != nil {
		t.Fatalf("recovery after restart must return nil: %v", err)
	}
	if n := probes.Load(); n != 1 {
		t.Fatalf("exactly one probe after the deadline, got %d", n)
	}
	s := second.snapshot()
	if s.Phase != "plan" || !s.RemoteOutageSince.IsZero() || s.RemoteProbeCount != 0 {
		t.Fatalf("recovery after restart must reset the episode: %+v", s)
	}
}

// AC-7: a restarted supervisor with a published PR reuses the existing
// branch PR instead of creating a second one: the gh stub records that
// `pr list` is consulted and `pr edit` (not `pr create`) runs.
func TestRestartDuringOutageNoDuplicatePR(t *testing.T) {
	root := t.TempDir()
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
	// Bare origin plus a work clone on the issue branch with one extra commit.
	git(root, "init", "--quiet", "--bare", filepath.Join(root, "origin.git"))
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0700); err != nil {
		t.Fatal(err)
	}
	git(work, "init", "--quiet", "-b", "main")
	git(work, "remote", "add", "origin", filepath.Join(root, "origin.git"))
	commit(work, "initial", "file.txt", "one\n")
	git(work, "push", "origin", "main")
	git(work, "switch", "-c", branchFor(160))
	commit(work, "feature", "feature.txt", "feature\n")
	git(work, "push", "origin", branchFor(160))

	// gh stub: records every invocation; `pr list` reports the existing PR.
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0700); err != nil {
		t.Fatal(err)
	}
	ghCalls := filepath.Join(root, "gh-calls.log")
	stub := "#!/bin/sh\necho \"$@\" >> " + ghCalls + "\nif [ \"$1\" = \"pr\" ] && [ \"$2\" = \"list\" ]; then\n  echo '[{\"number\":77,\"state\":\"OPEN\"}]'\nfi\nexit 0\n"
	for _, tool := range []string{"gh", "pi"} {
		if err := os.WriteFile(filepath.Join(bin, tool), []byte(stub), 0700); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	stateDir := filepath.Join(root, "state")
	cfg := testConfig(stateDir)
	cfg.Repo = "HuddleScope/huddlescope.github.io"
	a, err := NewApp(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	a.checkout = work
	// The restart state carries the published identity plus an active outage
	// episode (the crash happened while waiting for the endpoint).
	since := time.Now().UTC().Add(-time.Minute)
	if err := a.update(func(s *State) {
		s.Issue = Issue{Number: 160, Title: "Feature", URL: "https://github.com/HuddleScope/huddlescope.github.io/issues/160"}
		s.Phase = "publish"
		s.MergePolicy = "manual"
		s.BaseBranch = "main"
		s.PRNumber = 77
		s.PublishedSHA = "unused-in-this-path"
		s.RemoteOutageSince = since
		s.RemoteHost = "mac-studio:8000"
		s.RemoteProbeCount = 2
		s.RemoteNotified = true
	}); err != nil {
		t.Fatal(err)
	}
	// publish computes against the live tree: the reviewed fingerprint must
	// match what the checkout holds now.
	fingerprint, err := a.diffFingerprint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.update(func(s *State) { s.ReviewedFingerprint = fingerprint }); err != nil {
		t.Fatal(err)
	}
	st := a.snapshot()
	pr, err := a.publish(ctx, st, st.Issue)
	if err != nil {
		t.Fatalf("publish must reuse the existing PR: %v", err)
	}
	if pr != 77 {
		t.Fatalf("publish must return the existing PR number, got %d", pr)
	}
	calls, err := os.ReadFile(ghCalls)
	if err != nil {
		t.Fatal(err)
	}
	text := string(calls)
	if n := countOccurrences(text, "pr create"); n != 0 {
		t.Fatalf("restart must not create a second PR: %d create calls:\n%s", n, text)
	}
	if n := countOccurrences(text, "pr edit 77"); n == 0 {
		t.Fatalf("restart must refresh the existing PR #77 instead of creating: %s", text)
	}
	if n := countOccurrences(text, "pr list"); n == 0 {
		t.Fatalf("publish must consult the branch PR before deciding: %s", text)
	}
	// The persisted outage episode survived the publish path untouched.
	if s := a.snapshot(); s.RemoteOutageSince != since || s.RemoteHost != "mac-studio:8000" || s.RemoteProbeCount != 2 {
		t.Fatalf("outage state must survive the publish path: %+v", s)
	}
}

func countOccurrences(s, sub string) int {
	n := 0
	from := 0
	for {
		i := indexOf(s[from:], sub)
		if i < 0 {
			return n
		}
		n++
		from += i + len(sub)
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// AC-7 (session guard): a second supervisor over the same state dir must
// fail on the exclusive lock instead of running a second session.
func TestSecondSupervisorLocked(t *testing.T) {
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
	stateDir := filepath.Join(dir, "state")
	cfg := testConfig(stateDir)
	cfg.Repo = "HuddleScope/huddlescope.github.io"
	first, err := NewApp(cfg, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	if _, err := NewApp(cfg, ""); err == nil {
		t.Fatal("second supervisor over the same state dir must fail on the lock")
	}
	// After the first releases, a new supervisor may start again.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := NewApp(cfg, "")
	if err != nil {
		t.Fatalf("supervisor must start after the lock is released: %v", err)
	}
	_ = second.Close()
}
