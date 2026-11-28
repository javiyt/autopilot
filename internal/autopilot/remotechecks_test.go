package autopilot

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestRemoteCheckShaBinding proves AC-2: a remote run is accepted only when its
// headSha equals the local HEAD; a matching success satisfies, a mismatched
// success blocks with a distinct reason, a non-success conclusion blocks, and
// a watch timeout blocks.
func TestRemoteCheckShaBinding(t *testing.T) {
	const headSHA = "0123456789abcdef0123456789abcdef01234567"

	mkRunner := func(runID, status, conclusion, headSha string) remoteCheckGhRunner {
		return remoteCheckGhRunner{
			Dispatch: func(ctx context.Context, checkout, workflow, ref string) (string, error) {
				return runID, nil
			},
			View: func(ctx context.Context, checkout, runID string) (string, error) {
				return `{"status":"` + status + `","conclusion":"` + conclusion + `","headSha":"` + headSha + `"}`, nil
			},
			PollInterval: time.Millisecond,
		}
	}

	t.Run("matching headSha + success satisfies", func(t *testing.T) {
		rc := RemoteChecks{Workflow: "arm64-ci.yml"}
		out := RunRemoteChecks(context.Background(), "/tmp", rc, headSHA, mkRunner("111", "completed", "success", headSHA))
		if !out.Satisfied || out.Cause != "ok" {
			t.Fatalf("want satisfied/ok, got %+v", out)
		}
		if out.HeadSHA != headSHA {
			t.Fatalf("HeadSHA = %q, want %q", out.HeadSHA, headSHA)
		}
	})

	t.Run("mismatched headSha blocks (distinct reason)", func(t *testing.T) {
		rc := RemoteChecks{Workflow: "arm64-ci.yml"}
		out := RunRemoteChecks(context.Background(), "/tmp", rc, headSHA, mkRunner("222", "completed", "success", "ffffffffffffffffffffffffffffffffffffffff"))
		if out.Satisfied {
			t.Fatal("mismatched headSha must not satisfy")
		}
		if out.Cause != "sha-mismatch" {
			t.Fatalf("cause = %q, want sha-mismatch", out.Cause)
		}
	})

	t.Run("non-success conclusion blocks", func(t *testing.T) {
		rc := RemoteChecks{Workflow: "arm64-ci.yml"}
		out := RunRemoteChecks(context.Background(), "/tmp", rc, headSHA, mkRunner("333", "completed", "failure", headSHA))
		if out.Satisfied {
			t.Fatal("non-success must not satisfy")
		}
		if out.Cause != "conclusion-failed" {
			t.Fatalf("cause = %q, want conclusion-failed", out.Cause)
		}
	})

	t.Run("dispatch failure blocks with distinct reason", func(t *testing.T) {
		rc := RemoteChecks{Workflow: "arm64-ci.yml"}
		runner := remoteCheckGhRunner{
			Dispatch: func(ctx context.Context, checkout, workflow, ref string) (string, error) {
				return "", errors.New("workflow not found")
			},
			PollInterval: time.Millisecond,
		}
		out := RunRemoteChecks(context.Background(), "/tmp", rc, headSHA, runner)
		if out.Satisfied || out.Cause != "dispatch-failed" {
			t.Fatalf("want dispatch-failed, got %+v", out)
		}
	})

	t.Run("not-configured is a distinct block", func(t *testing.T) {
		out := RunRemoteChecks(context.Background(), "/tmp", RemoteChecks{}, headSHA, remoteCheckGhRunner{})
		if out.Satisfied || out.Cause != "not-configured" {
			t.Fatalf("want not-configured, got %+v", out)
		}
	})

	t.Run("watch timeout blocks (distinct reason)", func(t *testing.T) {
		rc := RemoteChecks{Workflow: "arm64-ci.yml", TimeoutSeconds: 1}
		calls := 0
		runner := remoteCheckGhRunner{
			Dispatch: func(ctx context.Context, checkout, workflow, ref string) (string, error) {
				return "444", nil
			},
			View: func(ctx context.Context, checkout, runID string) (string, error) {
				calls++
				// never terminal
				return `{"status":"in_progress","conclusion":"","headSha":"` + headSHA + `"}`, nil
			},
			PollInterval: 10 * time.Millisecond,
		}
		start := time.Now()
		out := RunRemoteChecks(context.Background(), "/tmp", rc, headSHA, runner)
		if out.Satisfied || out.Cause != "timeout" {
			t.Fatalf("want timeout, got %+v", out)
		}
		if calls == 0 {
			t.Fatal("expected at least one poll before the deadline")
		}
		if time.Since(start) > 5*time.Second {
			t.Fatalf("polling loop ran longer than the bounded timeout (%s)", time.Since(start))
		}
	})
}

// TestRemoteChecksConfigValidation proves AC-6: enum / SHA validation of the
// runner config at load time (workflow name, full-SHA ref, non-negative
// timeout).
func TestRemoteChecksConfigValidation(t *testing.T) {
	validSHA := "0123456789abcdef0123456789abcdef01234567"

	// Empty = feature off, always valid.
	if err := validateRemoteChecks(RemoteChecks{}); err != nil {
		t.Fatalf("empty RemoteChecks must be valid, got %v", err)
	}
	// Valid workflow name + full SHA ref.
	if err := validateRemoteChecks(RemoteChecks{Workflow: "arm64-ci.yml", Ref: validSHA, TimeoutSeconds: 600}); err != nil {
		t.Fatalf("valid remote checks rejected: %v", err)
	}
	// .yaml extension also accepted.
	if err := validateRemoteChecks(RemoteChecks{Workflow: "arm64-ci.yaml"}); err != nil {
		t.Fatalf(".yaml workflow name rejected: %v", err)
	}
	// Path separator / metacharacter in the workflow name is rejected (it is
	// interpolated into a gh argument).
	for _, bad := range []string{"../evil.yml", "arm64-ci.yml;rm", "dir/arm64.yml", "arm64-ci"} {
		if err := validateRemoteChecks(RemoteChecks{Workflow: bad}); err == nil {
			t.Fatalf("unsafe workflow name %q accepted", bad)
		}
	}
	// A non-SHA ref (branch/tag/short) is rejected.
	for _, bad := range []string{"main", "abc123", "0123456789abcdef0123456789abcdef0123456" /* 39 */, "0123456789ABCDEF0123456789abcdef01234567" /* uppercase */} {
		if err := validateRemoteChecks(RemoteChecks{Workflow: "arm64-ci.yml", Ref: bad}); err == nil {
			t.Fatalf("non-SHA ref %q accepted", bad)
		}
	}
	// Negative timeout is rejected; zero (default) is fine.
	if err := validateRemoteChecks(RemoteChecks{Workflow: "arm64-ci.yml", TimeoutSeconds: -1}); err == nil {
		t.Fatal("negative timeout accepted")
	}
	if err := validateRemoteChecks(RemoteChecks{Workflow: "arm64-ci.yml", TimeoutSeconds: 0}); err != nil {
		t.Fatalf("zero timeout (default) rejected: %v", err)
	}
}
