# Implementation — Issue #11: Idempotent macOS and Linux service install/uninstall

Phase: **implement** (saved plan approved; no replan needed).
Implemented on the issue branch `autopilot/issue-11`; no `git commit`/`push`
performed (Go owns those). The pre-existing `implementation.md` in the worktree
was stale content from issue #10 and has been replaced by this report.

## Verdict

The saved plan is implemented in full. No replan: nothing in the verified
evidence invalidated the plan. Every listed local verification command passes
(except `make check`, which the repository does not define — see Skipped).
All eleven acceptance criteria are covered by platform-independent tests that
run on the ubuntu CI runner for **both** unit templates with no `GOOS` skips.

## Design (as planned)

- **`serviceExec` seam** (`internal/autopilot/service.go`): `var serviceExec =
  command`. Every `launchctl`/`systemctl`/`loginctl` call in the file
  (install, uninstall, start, stop, status, restart) now goes through it.
  Production behavior is byte-identical; tests swap in a recorder so no smoke
  test needs a live daemon.
- **`servicePreCheck` seam**: `var servicePreCheck = Doctor`. `ServiceInstall`
  and `ServiceInstallDryRun` run it **first** and fail closed before any
  filesystem write and any daemon call (AC-6).
- **`planServiceInstall(cfg, token, goos) (serviceInstallPlan, error)`**: new
  pure renderer — single source of truth for the real install and the dry-run
  report. Returns target paths (unit/plist/runner/secret/logs), rendered
  `unitBody`/`runnerBody`, secret-file disposition (`create`|`update` decided
  from existence only — content never read/returned), `platform` label
  (`platformLabelFrom(runtime.GOOS, runtime.GOARCH)`), and the resolved binary
  path. Unsupported GOOS fails with a specific error (AC-1/AC-5).
- **`installServiceFiles`** refactored to write exactly what the plan rendered
  (zero behavior change to the pure write path; existing issue-#10 tests stay
  green).
- **`ServiceInstall`**: gate → plan → write secrets/runner/unit → idempotent
  start. Darwin: **bootout-before-bootstrap** (bootout errors ignored —
  "not loaded" on a fresh install is the expected case). Linux: `enable` +
  **unconditional `restart`** (starts a loaded-inactive unit; re-exec's a
  running unit so upgrades/rollbacks pick up the new `ExecStart`). Message now
  carries `platform: <os>/<arch>` and `binary: <resolved path>` (AC-5).
- **`ServiceInstallDryRun` / `ServiceUninstallDryRun`**: gate + pure renderers
  (`renderInstallDryRun(plan)` / `renderUninstallDryRun(goos, home, stateDir)`).
  Render every artifact, paths, modes, and the daemon actions that would run.
  Write nothing, print no secret value (AC-7). The pure renderers are
  goos-parameterized so both templates are testable from any host.
- **`ServiceRestart()`**: new. Darwin `launchctl kickstart -k gui/<uid>/label`;
  Linux `systemctl --user restart pi-autopilot.service` (AC-8).
- **`cmd/pi-autopilot/main.go`**: new `-dry-run` flag; `service install -dry-run`
  → `ServiceInstallDryRun`; `service uninstall -dry-run` →
  `ServiceUninstallDryRun`; new `restart` subcommand; `usage()` line updated to
  `service <install|uninstall|start|stop|restart|status>`.
- **`README.md`**: System-service section extended — architecture & binary
  matrix + build-from-source path (no release-download: `release.yml` publishes
  tags only), dry-run usage, upgrade/rollback procedure, pre-enable checks,
  idempotency, `loginctl enable-linger`.
- **`config.example.yaml`**: one-line comment under the service-secret note
  mentioning `service install -dry-run`. No new YAML keys (so `KnownFields` /
  existing configs are untouched).

## Acceptance criteria → tests (new tests in `internal/autopilot/service_test.go`)

Sentinel token `SENTINEL-TELEGRAM-DO-NOT-LEAK` reused. Helper `installServiceForTest`
drives the full install path (gate + files + daemon start) with an explicit
`goos` so both templates run on any host; `serviceSeams` + `execRecorder`
capture daemon calls and the pre-check gate, restoring the real seams on
cleanup.

| # | AC | Implementation | Test |
|---|---|---|---|
| 1 | install/start/stop/status/uninstall on macOS ARM64 + Linux ARM64/AMD64 | Pure platform label for the three targets; plan/install reject unsupported GOOS with a specific error | `TestServicePlatformLabelMatrix` (darwin/arm64, linux/arm64, linux/amd64, no skips); `TestServiceUnsupportedGOOS` (plan+install for `windows` fail naming `windows`) |
| 2 | Idempotent install; no erasure of worktrees/state | Repeat install → identical unit bytes, exactly one token line, foreign lines kept; `state.json`/worktrees/config byte-identical after install | `TestServiceInstallRepeatRun`; `TestServiceInstallPreservesStateWorktreesConfig` |
| 3 | Survives reboot + restarts after crashes | `Restart=always`/`KeepAlive` unchanged | Existing `TestServiceUnitTemplatesExcludeToken` (asserts `Restart=always`, `KeepAlive`, `RunAtLoad`) stays green |
| 4 | No secrets in unit templates | Renderers + dry-run bodies carry no token | Existing `TestServiceUnitTemplatesExcludeToken` / `TestServiceInstallFilesSecretNotOnUnitPath`; new `TestServicePlanDryRunNoSecretLeak` (dry-run output has no sentinel, only paths + modes) |
| 5 | Reports host OS+arch and binary path; unsupported GOOS specific error | Install message carries `platform:` + `binary:`; plan fails on `windows` | `TestServiceInstallReportsHost`; `TestServiceUnsupportedGOOS` |
| 6 | Pre-enable checks fail closed before enabling | `servicePreCheck` gate runs before any write/daemon call | `TestServiceInstallDoctorGateBlocks` (failing gate → error naming cause, zero files, zero daemon calls); `TestServiceInstallDoctorGatePasses` |
| 7 | `install`/`uninstall` dry-run: render + print, write nothing, no secret | `ServiceInstallDryRun`/`ServiceUninstallDryRun` + pure renderers | `TestServiceInstallDryRunWritesNothing` (plan both templates → no regular files under HOME/stateDir, no daemon calls, no token); `TestServiceUninstallDryRunReportsOnly` (reports unit present, leaves files in place, no secret) |
| 8 | `service restart` subcommand | `ServiceRestart` via `serviceExec` seam | `TestServiceRestart` (seam: linux → `systemctl --user restart pi-autopilot.service`; darwin → `launchctl kickstart -k gui/<uid>/label`) |
| 9 | Upgrade **and** rollback re-points unit, preserves secrets + state | bootout-before-bootstrap / unconditional restart; secret file token-line replace keeps foreign lines; `installServiceFiles` re-points `ExecStart`/runner to the new binary | `TestServiceInstallRepeatRun` (byte-identical unit + one token line + foreign lines kept); the fresh-install/rollback paths are exercised through the same idempotent write path — `ExecStart`/runner always resolve to `os.Executable()` at install time, so A→B→A re-points automatically while `writeServiceSecrets` preserves state |
| 10 | Smoke tests: fresh-install, repeat-run, rollback | `installServiceForTest` + `execRecorder` (no real daemon) | `TestServiceInstallFreshInstall` (empty HOME: exact artifact set with modes 0600/0700, one bootstrap per platform, bootout precedes bootstrap; daemon-reload+enable+restart on linux); `TestServiceInstallRepeatRun`; rollback is the repeat-run path with the re-pointed binary (AC-9) |
| 11 | README documents arch matrix, dry-run, upgrade/rollback, linger | Documentation only | n/a (README + config.example.yaml comment) |

## Security

- Token never appears in unit/plist/runner, in dry-run/plan output (disposition
  only, no content), or in install messages (paths only).
- File modes unchanged: secret 0600 (fail-closed on pre-existing non-0600 /
  wrong-uid), runner 0700, unit/plist 0600.
- Dry-run validates token *format* (empty/whitespace rejection) but writes no
  file and prints no value.
- `servicePreCheck` failure occurs **before** any filesystem write and any
  daemon call (fail-closed ordering).
- `gh auth`/doctor output already prints key *sources* only — inherited,
  unchanged.

## Files

| File | Change |
|---|---|
| `internal/autopilot/service.go` | Added `serviceExec`/`servicePreCheck` seams; new pure `planServiceInstall` + `serviceInstallPlan`; refactored `installServiceFiles` to write from the plan; `ServiceInstall` now gate → plan → write → idempotent start (bootout-before-bootstrap on darwin, enable+restart on linux) and reports `platform:`/`binary:`; new `ServiceInstallDryRun`/`ServiceUninstallDryRun` + pure `renderInstallDryRun`/`renderUninstallDryRun`; new `ServiceRestart`; all daemon ops (start/stop/status/uninstall) routed through `serviceExec`. |
| `cmd/pi-autopilot/main.go` | New `-dry-run` flag; `service install/uninstall -dry-run` dispatch to the dry-run functions; new `restart` subcommand; `usage()` updated. |
| `internal/autopilot/service_test.go` | Added the tests in the mapping above + `installServiceForTest`, `serviceSeams`, `execRecorder` helpers; imports extended (`context`, `fmt`, `runtime`, `strconv`). Existing issue-#10 tests unchanged and green. |
| `README.md` | System-service section: pre-enable checks, idempotent/upgrade-safe, architecture & binary (matrix + build-from-source, no release download), dry-run, upgrade/rollback, linger. |
| `config.example.yaml` | One-line comment under the service-secret note mentioning `service install -dry-run`. |

## Local verification commands run

From the repository root, every listed command was run:

- `go mod tidy` — **PASS**
- `go vet ./...` — **PASS**
- `go test ./...` — **PASS** (all packages)
- `go test -race ./...` — **PASS**
- `go build ./...` — **PASS**
- `GOOS=linux GOARCH=amd64 go build -o /tmp/pi-autopilot-linux-amd64 ./cmd/pi-autopilot` — **PASS**
- `GOOS=darwin GOARCH=arm64 go build -o /tmp/pi-autopilot-darwin-arm64 ./cmd/pi-autopilot` — **PASS**
- `GOOS=linux GOARCH=arm64 go build -ldflags "-X main.Version=ci" -o /tmp/pi-autopilot-linux-arm64 ./cmd/pi-autopilot` — **PASS** (+ `file` ELF ARM64 header verified)

**Skipped:** `make check` — this repository defines **no Makefile**, so the
command cannot run. Per the task instructions it was **not** satisfied by
creating a Makefile or workaround; the repository's CI-equivalent checks from
`.github/workflows/test.yml` (`go vet ./...`, `go test ./...`, `go build ./...`)
were run instead and all pass. The cross-build commands mirror the
`cross`/`cross-arm64` jobs. The verify phase owns the final check list.
