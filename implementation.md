# Implementation — Issue #8: resolve local verification commands deterministically whenever possible

Phase: **implement** (plan approved; no replan needed).
Implemented on the issue branch; no `git commit`/`push` performed (Go owns those).

## Verdict

The saved plan is implemented in full. No replan: nothing in the verified
evidence invalidated the plan. The one environment fact confirmed before
coding — the repository has **no Makefile** — is handled by the plan's own
rollout rule ("a make target while no Makefile exists" → resolver emits
nothing, model path unchanged), not by a workaround or a new file.

All acceptance criteria (AC-1…AC-6) are satisfied and covered by tests that
pass under `go test -race ./...`.

## Files

| File | Change |
|---|---|
| `internal/autopilot/checks.go` (new) | The deterministic core: `resolveDeterministicChecks`, `filterToAllowedRoots`, `redactSecrets`, `collectCheckSecrets`, and `runCheck` (bounded-timeout check runner returning output + exit code + timeout cause). Pure / filesystem-only, fully unit-testable. |
| `internal/autopilot/checks_test.go` (new) | Resolver fixtures (Go, Node, Makefile, GitHub Actions, no-file escape valve, hostile target name), `TestFilterToAllowedRoots`, `TestRedactCheckLog`, `TestRunCheckEvidence`. |
| `internal/autopilot/steps.go` | `selectVerifyChecks` now takes the four sources (configured, resolved, plan, model) and returns per-check `source` tags (`config`/`resolved`/`plan`/`verify`); plan checks are always allowlist-filtered (the asymmetry gap is closed). The `local_verify` phase resolves deterministically **first** and consults the verify model **only** when the resolver finds nothing; checks execute via `runCheck` with exit code / timeout evidence and redacted logs. |
| `internal/autopilot/steps_test.go` | `TestSelectVerifyChecks` extended to the new 4-source signature with source-tag assertions; new `TestSelectVerifyChecksDeterministicFirst` (AC-1 no-LLM proof at the selection layer) and `TestPlanChecksFiltered` (untrusted plan data never runs). |
| `internal/autopilot/workflow.go` | The investigate baseline now runs configured **plus** resolved checks (so the pre-implementation baseline covers exactly the set `local_verify` executes), uses `runCheck` for exit-code/timeout evidence, and redacts baseline logs. `BaselineCheck` keying unchanged. |
| `internal/autopilot/app.go` | Added `telegramTokenOf` helper (returns the token value only for log redaction; never logged). |
| `README.md` | `local_verify` stage row updated: deterministic resolution first, model only when no discoverable entrypoint; redaction note. |
| `config.example.yaml` | `check_commands` and `verify` comments refreshed to the new precedence and the "model no longer re-decides a resolvable repo" behavior. |

## Acceptance criteria

- **AC-1** (no LLM for resolvable repos): `local_verify` calls
  `resolveDeterministicChecks` before the model; the model is invoked *only*
  when the resolver returns `ok=false`. `TestSelectVerifyChecksDeterministicFirst`
  proves the model-derived list is ignored when the resolver hit.
- **AC-2** (deterministic dedupe + allowlist): `selectVerifyChecks` dedupes in
  the stable order configured → resolved → plan → model; every executed check
  passes `filterToAllowedRoots` (plan commands are filtered on every path).
  `TestSelectVerifyChecks`, `TestFilterToAllowedRoots`, `TestPlanChecksFiltered`.
- **AC-3** (bounded timeout, exit code, full redacted logs): `runCheck`
  records the child exit code (124 on timeout, 125 on launch failure) and a
  `timedOut` flag; `local_verify` and the baseline store the evidence with an
  `exit code:` / `cause: timeout` line, redacted via `redactSecrets`.
  `TestRunCheckEvidence`, `TestRedactCheckLog`.
- **AC-4** (unknown/missing never green): an empty final list blocks the phase
  (existing error retained and extended); a check whose entrypoint cannot run
  is a FAIL (non-zero exit code); a baseline failure is a "baseline blocker",
  and the baseline set now includes resolved checks so a resolved check that
  failed pre-implementation is a baseline blocker, not a fresh rework.
  `TestRunCheckEvidence/missing binary`, the `local_verify` empty-list branch.
- **AC-5** (baseline, fingerprints, rework unchanged in semantics):
  `local-fingerprint.txt`, diff fingerprint, rework transitions and
  `MaxLocalFixes` accounting are untouched; `local-checks.md` records each
  executed check's source (`config`/`resolved`/`plan`/`verify`).
- **AC-6** (CI green): `go mod tidy`, `go test -race ./...`, `go vet ./...`
  all pass with the new tests.

## Local verification commands run

The task listed `make check` as a local verification command. **This repository
defines no Makefile**, so `make check` cannot run; per the task instructions it
was **not** satisfied by creating a Makefile or workaround. The repository's
CI-equivalent checks (from `.github/workflows/test.yml`) were run instead:

- `go mod tidy` — **PASS**
- `go test -race ./...` — **PASS** (all packages)
- `go vet ./...` — **PASS**

Skipped (entrypoint not defined in this repo): `make check`.

The deterministic resolver (`resolveDeterministicChecks`) correctly detects this
repository's Go entrypoints from `go.mod` (`go build ./...`, `go vet ./...`,
`go test -race ./...`), mirroring the CI definition, and would fall back to the
verify model only for a repository with no discoverable entrypoint.
