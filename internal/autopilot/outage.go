package autopilot

import (
	"strconv"
	"strings"
	"time"
)

// Remote inference outage handling (issue #6).
//
// When the remote inference endpoint the stage profile targets goes down,
// the stage must wait and probe with bounded backoff instead of burning the
// code-fix retry counter, and resume automatically in a fresh Pi session
// when the endpoint returns. Provider OOM/context failures are bounded in a
// separate counter; local Pi exits and task failures keep the existing
// retry→block behavior of fail().

// FailureClass is the supervisor's classification of a stage failure.
type FailureClass string

const (
	// FailureRemoteOutage: connection failure (preflight unreachable or an
	// in-flight connection drop). The endpoint is expected to self-heal:
	// wait with bounded backoff and probe; never consume stage retries.
	FailureRemoteOutage FailureClass = "remote-outage"
	// FailureOOM: provider out-of-memory / context exhaustion. Bounded in a
	// separate counter (maxOOMRetries) with its own distinct block message;
	// does not consume MaxStageRetries.
	FailureOOM FailureClass = "oom"
	// FailureExecution: local Pi exits, loop detection, idle timeout,
	// stopReason=error, gate errors, and credential rejection (auth-failed:
	// a rejected credential does not self-heal; the operator must act).
	// Kept on the existing code-fix retry→block path (fail()).
	FailureExecution FailureClass = "execution"
)

// maxOOMRetries bounds the separate OOM counter before the phase blocks with
// an OOM-specific message (the operator should lower load or change model).
const maxOOMRetries = 5

// Recovery timing. Package variables (not config keys in v1) so the pure
// backoff sequences stay table-testable at their production values while a
// test can shrink them; the defaults are the documented production floors.
var (
	outageBackoffBase    = 30 * time.Second
	outageBackoffCap     = 60 * time.Minute
	oomBackoffBase       = 2 * time.Minute
	oomBackoffCap        = 30 * time.Minute
	outageReminderPeriod = 1 * time.Hour
	stageRetryDelay      = 5 * time.Second
)

// nextBackoff returns the probe delay before recovery attempt count (1-based):
// 30s, 1m, 2m, … doubling, capped at 60 min. Count <= 0 yields the base.
func nextBackoff(count int) time.Duration {
	if count < 1 {
		count = 1
	}
	d := outageBackoffBase
	for i := 1; i < count && d < outageBackoffCap; i++ {
		d *= 2
		if d > outageBackoffCap {
			d = outageBackoffCap
		}
	}
	if d > outageBackoffCap {
		d = outageBackoffCap
	}
	return d
}

// oomBackoff is the retry delay after the nth OOM failure: 2m, 4m, … capped
// at 30 min. n <= 0 yields the base.
func oomBackoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	d := oomBackoffBase
	for i := 1; i < n && d < oomBackoffCap; i++ {
		d *= 2
		if d > oomBackoffCap {
			d = oomBackoffCap
		}
	}
	if d > oomBackoffCap {
		d = oomBackoffCap
	}
	return d
}

// executionMarkers are deterministic stage failure texts that must keep the
// code-fix retry semantics no matter what generic words they contain: "idle
// timeout" matches the transient timeout regex but is not a connectivity
// event, and loop/gate/stopReason failures are task failures, not outages.
var executionMarkers = []string{
	"idle timeout",
	"repetition loop detected",
	"no final assistant text",
	"assistant message reported a failure",
	"pass limit reached",
}

// ClassifyStageError is the single source of truth for which failure class a
// stage failure belongs to. preflight is the remotePreflight probe result of
// the failed stage (nil when no remote endpoint was probed for it). Rules:
//  1. Preflight unreachable            -> remote-outage
//  2. Preflight auth-failed            -> execution (credential does not self-heal)
//  3. Deterministic stage failure text -> execution (loop, idle timeout, stopReason, gate)
//  4. Pi failure connection/transient  -> remote-outage (server died mid-session)
//  5. Pi failure memory/context or
//     process-killed (possible OOM)    -> oom
//  6. Everything else                  -> execution (today's behavior)
func ClassifyStageError(err error, preflight *RemoteCheckResult) FailureClass {
	if preflight != nil {
		switch preflight.Status {
		case StatusUnreachable:
			return FailureRemoteOutage
		default:
			// Auth or model-not-found pre-flight states are operator errors.
			return FailureExecution
		}
	}
	if err == nil {
		return FailureExecution
	}
	if msg := err.Error(); msg != "" {
		for _, marker := range executionMarkers {
			if strings.Contains(msg, marker) {
				return FailureExecution
			}
		}
	}
	switch kind := failureKindOf(err); kind {
	case "connection/transient":
		return FailureRemoteOutage
	case "memory/context", "process-killed (possible OOM)":
		return FailureOOM
	}
	return FailureExecution
}

// failureKindOf extracts the PiResult.FailureKind embedded in a stage error.
// runStageIn wraps PiResult.Err as "%s (log: %s)"; RunPi embeds the kind as
// "%w (%s): %s". For errors produced outside RunPi the text still carries
// the kind only when it was embedded; otherwise the string is re-classified
// with ClassifyFailure so mid-run provider errors surface correctly.
func failureKindOf(err error) string {
	text := err.Error()
	for _, kind := range []string{"connection/transient", "memory/context", "process-killed (possible OOM)"} {
		if strings.Contains(text, "("+kind+")") {
			return kind
		}
	}
	return ClassifyFailure(text)
}

// outageDecision is the pure notification/dedup logic for one probe round of
// the outage wait loop. It decides which transition messages are owed given
// the persisted dedup state and the probe outcome; the caller sends them and
// persists the returned flags. time.Now is injected so the sequence is
// deterministic in tests.
type outageDecision struct {
	NotifyOffline   bool // endpoint went offline (once per outage episode)
	OfflineMessage  string
	NotifyOnline    bool // endpoint recovered; the stage may resume
	OnlineMessage   string
	NotifyReminder  bool // hourly "still offline" reminder
	ReminderMessage string
	// Persisted dedup state after this round:
	Notified     bool
	ReminderAt   time.Time
	ProbeCount   int
	BackoffUntil time.Time
	OutageSince  time.Time
	// ResetOutage is true once recovery is confirmed: the caller clears all
	// outage fields before resuming the stage in a fresh session.
	ResetOutage bool
}

// decideOutage computes the decision for one probe round. state holds the
// persisted outage fields; healthy is the probe outcome (true when every
// probeable endpoint reported ok); probeCount is the count AFTER this
// round's probe; now is the current time. host is host:port only — never a
// key value (repo invariant).
func decideOutage(host, phase string, state State, healthy bool, probeCount int, now time.Time) outageDecision {
	d := outageDecision{Notified: state.RemoteNotified, ReminderAt: state.RemoteReminderAt, ProbeCount: probeCount}
	if healthy {
		// Online transition: notify exactly once per outage episode (the
		// offline message was sent when Notified became true), then reset.
		if state.RemoteOutageSince.IsZero() {
			// No persisted episode (e.g. first probe already healthy): nothing owed.
			return d
		}
		d.NotifyOnline = true
		d.OnlineMessage = "✅ " + host + " back online; resuming " + phase + " with a fresh session"
		d.ResetOutage = true
		d.Notified = false
		d.ReminderAt = time.Time{}
		d.OutageSince = time.Time{}
		d.BackoffUntil = time.Time{}
		d.ProbeCount = 0
		return d
	}
	// Offline path.
	if state.RemoteOutageSince.IsZero() {
		d.OutageSince = now
	} else {
		d.OutageSince = state.RemoteOutageSince
	}
	if probeCount < 1 {
		d.ProbeCount = 1
	} else {
		d.ProbeCount = probeCount
	}
	if state.RemoteNotified {
		d.Notified = true
	} else {
		d.NotifyOffline = true
		d.OfflineMessage = "⚠️ Inference endpoint " + host + " unreachable while running " + phase +
			"; waiting — I resume automatically when it returns"
		d.Notified = true
	}
	// Backoff extends from the latest persisted until, or from now.
	base := now
	if !state.RemoteBackoffUntil.IsZero() && state.RemoteBackoffUntil.After(now) {
		base = state.RemoteBackoffUntil
	}
	d.BackoffUntil = base.Add(nextBackoff(d.ProbeCount))
	// Hourly reminder: first due at outage start + 1h, then every 1h.
	if state.RemoteReminderAt.IsZero() {
		d.ReminderAt = d.OutageSince.Add(outageReminderPeriod)
	} else {
		d.ReminderAt = state.RemoteReminderAt
	}
	if !d.ReminderAt.IsZero() && !now.Before(d.ReminderAt) {
		d.NotifyReminder = true
		d.ReminderMessage = "⚠️ " + host + " still offline (" + phase + " waiting, outage " +
			outageElapsed(d.OutageSince, now) + "); retrying with bounded backoff"
		d.ReminderAt = d.ReminderAt.Add(outageReminderPeriod)
	}
	return d
}

func outageElapsed(since, now time.Time) string {
	d := now.Sub(since)
	if d >= 24*time.Hour {
		return strconv.FormatInt(int64(int(d.Hours())), 10) + "h"
	}
	return strconv.FormatInt(int64(int(d.Minutes())), 10) + "m"
}
