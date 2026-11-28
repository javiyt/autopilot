package autopilot

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// The explicit remote-check runner (issue #9, AC-6). It dispatches an
// operator-named same-repo GitHub Actions workflow pinned to the exact HEAD
// SHA, polls it under a bounded timeout, and accepts its result ONLY when the
// remote run's headSha equals local HEAD. Any other outcome — a dispatch
// failure, a still-running deadline, a non-success conclusion, or a headSha
// mismatch — is a distinct block reason. It never bypasses other gates: the
// caller consults it only for checks it already recorded BLOCKED.

const remoteCheckTimeout = 30 * time.Minute

// remoteCheckOutcome is the classification of one remote-check dispatch.
// Satisfied is true only for the exact-commit success case. Cause is a stable
// machine-readable reason for every non-satisfied outcome so the gate can
// record distinct evidence and route like a failure.
type remoteCheckOutcome struct {
	Satisfied bool
	Cause     string // "ok" | "dispatch-failed" | "timeout" | "conclusion-failed" | "sha-mismatch" | "unparseable"
	Detail    string // human-readable, actionable; never contains secrets
	RunID     string // the dispatched Actions run id, when known
	HeadSHA   string // the remote run's headSha, when reported
}

// ghRunCreateOut parses the `gh run create` stdout, which is the dispatched
// run id on success.
var ghRunID = regexp.MustCompile(`^([0-9]+)\s*$`)

// ghRunnerDispatch is the seam for the `gh run create` dispatch so tests can
// substitute a fake without touching the network. The production default runs
// the real `gh` binary against the checkout.
type ghRunnerDispatch func(ctx context.Context, checkout, workflow, ref string) (string, error)

// ghRunView is the seam for the `gh run view --json` poll.
type ghRunView func(ctx context.Context, checkout, runID string) (string, error)

// remoteCheckGhRunner holds the dispatch/view seams and a poll interval. A nil
// zero value uses the real `gh` binary.
type remoteCheckGhRunner struct {
	Dispatch     ghRunnerDispatch
	View         ghRunView
	PollInterval time.Duration
}

func defaultRemoteChecker() remoteCheckGhRunner {
	return remoteCheckGhRunner{
		Dispatch: func(ctx context.Context, checkout, workflow, ref string) (string, error) {
			args := []string{"run", "create", workflow, "--ref", ref}
			out, err := command(ctx, checkout, "gh", args...)
			if err != nil {
				return "", err
			}
			id := strings.TrimSpace(out)
			if m := ghRunID.FindStringSubmatch(id); m != nil {
				return m[1], nil
			}
			return id, nil
		},
		View: func(ctx context.Context, checkout, runID string) (string, error) {
			return command(ctx, checkout, "gh", "run", "view", runID, "--json", "status,conclusion,headSha")
		},
		PollInterval: 15 * time.Second,
	}
}

// resolveRemoteCheckTimeout returns the bound for a remote-check wait, applying
// the default when unset.
func resolveRemoteCheckTimeout(rc RemoteChecks) time.Duration {
	if rc.TimeoutSeconds > 0 {
		return time.Duration(rc.TimeoutSeconds) * time.Second
	}
	return remoteCheckTimeout
}

// RunRemoteChecks dispatches the configured same-repo workflow pinned to the
// exact head SHA, polls it under a bounded timeout, and classifies the result.
// It returns the outcome; the caller decides how to use it (satisfied checks
// the arch-blocked set; any other outcome blocks the phase like a failure).
//
// Security invariants (issue #9):
//   - the ref dispatched is the caller-supplied full head SHA (40-hex,
//     validated upstream); a branch/tag is never dispatched
//   - the result is accepted only when conclusion == "success" AND the run's
//     headSha equals the local head SHA — a tampered/re-run or different-branch
//     result is a distinct sha-mismatch block
//   - no elevated permissions are requested; the workflow is operator-named
//     in config and lives in the same repository
//   - the whole wait is bounded (timeout_seconds / default)
func RunRemoteChecks(ctx context.Context, checkout string, rc RemoteChecks, headSHA string, runner remoteCheckGhRunner) remoteCheckOutcome {
	if runner.Dispatch == nil {
		runner = defaultRemoteChecker()
	}
	if rc.Workflow == "" {
		// No runner configured: an explicit block reason, never a vacuous pass.
		return remoteCheckOutcome{Satisfied: false, Cause: "not-configured", Detail: "remote_checks.workflow is not configured; an architecture-constrained check cannot be satisfied remotely"}
	}
	ref := rc.Ref
	if ref == "" {
		ref = headSHA
	}
	// Dispatch the same-repo workflow pinned to the exact commit.
	runID, err := runner.Dispatch(ctx, checkout, rc.Workflow, ref)
	if err != nil {
		return remoteCheckOutcome{
			Satisfied: false,
			Cause:     "dispatch-failed",
			Detail:    fmt.Sprintf("gh run create %s --ref %s failed: %v (does the workflow exist and is dispatch permitted on this repo?)", rc.Workflow, ref, err),
		}
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return remoteCheckOutcome{Satisfied: false, Cause: "unparseable", Detail: "gh run create returned no run id"}
	}

	timeout := resolveRemoteCheckTimeout(rc)
	deadline := time.Now().Add(timeout)
	// Bound the polling loop explicitly (not just via ctx) so a hang in `gh`
	// still produces a distinct timeout block.
	ticker := time.NewTicker(runner.PollInterval)
	defer ticker.Stop()

	for {
		out, err := runner.View(ctx, checkout, runID)
		if err == nil {
			if status, conclusion, headSha, ok := parseRunView(out); ok {
				// A terminal run: classify. Non-success or sha-mismatch are
				// distinct blocks; only success AND headSha==local is accepted.
				if status == "completed" {
					if conclusion != "success" {
						return remoteCheckOutcome{Satisfied: false, Cause: "conclusion-failed", Detail: fmt.Sprintf("remote run %s concluded %q (want success)", runID, conclusion), RunID: runID, HeadSHA: headSha}
					}
					if headSha != headSHA {
						return remoteCheckOutcome{Satisfied: false, Cause: "sha-mismatch", Detail: fmt.Sprintf("remote run %s headSha %s != local HEAD %s; result is not bound to this commit", runID, headSha, headSHA), RunID: runID, HeadSHA: headSha}
					}
					return remoteCheckOutcome{Satisfied: true, Cause: "ok", Detail: fmt.Sprintf("remote run %s succeeded at headSha %s", runID, headSha), RunID: runID, HeadSHA: headSha}
				}
				// still in progress (queued/in_progress/waiting/action_required)
			}
		}
		if time.Now().After(deadline) {
			return remoteCheckOutcome{Satisfied: false, Cause: "timeout", Detail: fmt.Sprintf("remote run %s did not reach a terminal state within %s", runID, timeout), RunID: runID}
		}
		if ctx.Err() != nil {
			return remoteCheckOutcome{Satisfied: false, Cause: "timeout", Detail: "context cancelled while waiting for remote run " + runID, RunID: runID}
		}
		select {
		case <-ctx.Done():
			return remoteCheckOutcome{Satisfied: false, Cause: "timeout", Detail: "context cancelled while waiting for remote run " + runID, RunID: runID}
		case <-ticker.C:
		}
	}
}

// parseRunView extracts status, conclusion and headSha from the
// `gh run view --json status,conclusion,headSha` object. ok=false when the
// output is not a parsable run object (the caller treats that as non-terminal
// and keeps polling to the deadline).
func parseRunView(out string) (status, conclusion, headSha string, ok bool) {
	var run struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
		HeadSHA    string `json:"headSha"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &run); err != nil {
		return "", "", "", false
	}
	return run.Status, run.Conclusion, run.HeadSHA, true
}

// ensureRemoteCheckDeps verifies the `gh` binary exists before dispatch; a
// missing gh is a distinct block (the runner cannot operate without it).
func ensureRemoteCheckDeps() error {
	if _, err := exec.LookPath("gh"); err != nil {
		return fmt.Errorf("gh is not on PATH; the remote-check runner cannot dispatch without it")
	}
	return nil
}
