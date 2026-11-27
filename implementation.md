# Implementation — Issue #7: Allowlisted Pi web-search extension in managed stage sessions

Phase: **implement** (plan approved; no replan needed).
Implemented on the issue branch; no `git commit`/`push` performed (Go owns those).

## Verdict

The saved plan is implemented in full with one **design deviation in the
install-location mechanism** that the plan explicitly contemplated as a
fallback (plan §6 Risk #1 / §8). No replan: the §8 pre-implementation
verification step confirms the real `pi` 1.6.0 **does honor
`PI_CODING_AGENT_DIR`** for `pi install`, so the primary path works; the
deviation is only *where* inside the managed dir the installed package lives
(npm packages land under `<agent_dir>/npm/node_modules/<name>`, not
`<agent_dir>/extensions/<name>`), resolved by an **install → copy** step into
a stable supervisor-owned entry path.

All acceptance criteria (AC-1…AC-4) are satisfied and covered by tests that
pass under `go test -race ./...`.

## Pre-implementation verification (plan §8 pre-step — done)

Against the real pi 1.6.0 (Homebrew Cellar,
`/opt/homebrew/Cellar/pi-coding-agent/1.6.0/...`):

- `pi --help` confirms `install <source>`, `-e/--extension`, `--tools`,
  `--no-approve`, `--append-system-prompt`, `--no-extensions`.
- Live `PI_CODING_AGENT_DIR=$(mktemp -d) pi install npm:left-pad@1.3.0`
  **honored** `PI_CODING_AGENT_DIR` (install + `settings.json` declaration
  stayed inside the temp agent dir; global `~/.pi` untouched).
- Install location: npm → `<agent dir>/npm/node_modules/<name>` (scoped
  `@scope/name` under `node_modules`). Exact-pinned npm specs are not
  re-fetched; `--no-approve` ignores trust-gated project config.
- Explicit `-e <path>` loads even under `--no-extensions` (only
  `cliEnabledExtensions` load; discovery is disabled). Directories resolve via
  `package.json` `pi.extensions` or `index.js`/`index.ts` discovery.

Because npm installs land under `npm/node_modules` (not `extensions/`), the
extension entry is resolved by **copying** the installed package into a stable
`<agent_dir>/extensions/pi-websearch/` path whose content hash is the
supervisor-verified value. This is the plan's §6 Risk #1 fallback and keeps
the `-e` path stable and deterministic regardless of the registry's internal
layout.

## Files changed

| File | Change |
|---|---|
| `internal/autopilot/types.go` | New `WebSearch` struct + `Config.WebSearch` field (yaml `web_search`). `ReadConfig` captures `rawAPIKey` pre-expansion, expands env vars in `WebSearch` string fields, runs `validateWebSearch`, and applies the default timeout (120) when enabled+unset. |
| `internal/autopilot/websearch.go` | **New.** Pin validation (exact npm version / 40-hex git commit; reject `@latest`, ranges, tags, branches), role-policy validation (read-only set), `WebSearchRoleAllowed`, `webSearchEntry` (fail-closed `-e` resolution for `RunPi`), `SyncWebSearchExtension` (idempotent install→copy→hash→manifest; tamper ⇒ purge+error; disabled ⇒ purge), `hashDir` (deterministic content-only sha256), `copyDir` (symlink/mode-preserving), `purgeWebSearch`, `webSearchEnv` (`PI_AUTOPILOT_WEBSEARCH_*` child-env entries; env refs pass through, literals injected as `PI_AUTOPILOT_WEBSEARCH_KEY_<BACKEND>`), `webSearchKeyEnvVarName`, `webSearchKeySource`, `webSearchUntrustedDirective`. |
| `internal/autopilot/process.go` | `RunPi`: added `--no-approve` to the base args for **all** sessions (defense in depth). Built `--tools` string; for an allowlisted role resolves the verified `-e` entry (fail-closed), appends `-e <path>`, the extension's `web_search` tool to `--tools`, and the untrusted-data `--append-system-prompt` directive. |
| `internal/autopilot/piconfig.go` | `SyncManagedPi` now always runs `SyncWebSearchExtension` after the (idempotent) models.json write — keyed to `web_search` config, not the registry. `PiProcessEnv` appends `webSearchEnv(cfg)`. |
| `internal/autopilot/app.go` | `runStageIn` web-search pre-flight after the remote pre-flight: an allowlisted role re-syncs the extension; any error fails the stage (`stageError`) — never a silent fallback to extension-free. |
| `internal/autopilot/doctor.go` | `ManagedDoctor` reports web-search state: enabled/source, verified sha256 prefix, and key *source* only (never the value). |
| `config.example.yaml` | Commented-out `web_search` block with SearXNG example + guarantees. |
| `.env.example` | `SEARXNG_API_KEY` placeholder + `PI_AUTOPILOT_WEBSEARCH_*` note. |
| `README.md` | New "Web search for read-only stages" section: config block, guarantees, rollback. |
| `internal/autopilot/websearch_test.go` | **New.** 9 tests / subtests covering AC-1…AC-4 (fake-`pi` convention). |

## Design notes (what and why)

- **Default-off = today exactly.** `web_search` absent or `enabled: false`
  ⇒ `webSearchEnv` returns nil and `WebSearchRoleAllowed` is always false ⇒
  no `-e`, no env entries, `--tools` unchanged. The only argv delta on the
  default path is the newly-added `--no-approve` (plan §3 explicitly scopes
  this to *all* sessions as defense in depth; existing tests use substring
  assertions, so they remain green).
- **Containment.** Every session keeps
  `--no-extensions --no-mcp --no-skills --no-prompt-templates --no-approve`.
  An allowlisted role loads exactly one `-e` path from the *managed* agent
  dir (global `~/.pi` untouched — issue #4 isolation). The extension's tool
  is gated by the plain-name `--tools` allowlist, so a malicious extension's
  extra registered tools are never declared to the model.
- **Integrity (fail closed).** Source pinned (exact npm version / git commit)
  + `sha256` of the installed entry content. `SyncWebSearchExtension` installs
  the pin under `PI_CODING_AGENT_DIR`, copies into the stable entry dir,
  verifies the hash, and records `websearch.json` (atomic write + `.bak`,
  mirroring `models.json`). A mismatch or later tamper purges the install and
  errors; `RunPi` for an allowlisted role fails the session when the entry is
  missing/stale — no silent extension-free degradation.
- **Credentials.** `${VAR}` references are not injected — they pass through the
  inherited-environment merge in `piChildEnv`. A literal YAML key travels only
  in the child environment as `PI_AUTOPILOT_WEBSEARCH_KEY_<BACKEND>`, never in
  argv, on disk, or in logs. `doctor` prints key *sources* only.
- **No `State`/phase-machine/`RunPi`-signature change.** Everything derives
  from `cfg` + the already-passed `role`.

## Acceptance-criteria mapping → tests

| AC | Test (in `internal/autopilot/websearch_test.go`) | Status |
|---|---|---|
| AC-1 Phase activation | `TestWebSearchPhaseActivation` — `investigate` + enabled ⇒ argv has `--no-extensions` **and** `-e <managed path>`, `--tools` = `read,grep,find,ls,web_search`, `--append-system-prompt` present, `PI_AUTOPILOT_WEBSEARCH_*` + `PI_CODING_AGENT_DIR` in child env. | PASS |
| AC-2 Others extension-free | `TestWebSearchDisabledByDefault` — config absent **and** `enabled:false` ⇒ argv is exactly the baseline (no `-e`, same `--tools`, `--no-approve` present). | PASS |
| AC-2/3 Tool allowlist | `TestWebSearchToolAllowlist` — `implement`/`review` argv never has `-e` nor the extension tool; `--no-approve` present in all; `investigate` keeps `-e` as contrast. | PASS |
| AC-3 No policy bypass | `TestWebSearchRolePolicy` — `ReadConfig` rejects `allowed_roles` containing `implement`/`review` and empty-with-enabled; valid block loads and `WebSearchRoleAllowed` is correct. | PASS |
| AC-3 Integrity | `TestWebSearchIntegrity` — tamper installed entry ⇒ `SyncWebSearchExtension` errors + purges entry & manifest; an allowlisted `RunPi` then fails (no silent fallback). | PASS |
| AC-3 Credentials | `TestWebSearchCredentialSafety` — literal `api_key` ⇒ value appears exactly once in child env (`PI_AUTOPILOT_WEBSEARCH_KEY_SEARXNG`), never in argv, never in any managed-dir file; `${VAR}` ref ⇒ passes through inherited env only, nothing injected; `doctor` never contains the sentinel and prints the source line. | PASS |
| AC-4 Installer idempotent | `TestWebSearchInstallerIdempotent` — second sync with unchanged hash ⇒ no `pi install` re-run, manifest byte-identical. | PASS |
| AC-4 Pin/policy validation | `TestReadConfigWebSearch` — accept exact npm version + 40-hex git commit; reject `@latest`, `^`/`x` ranges, bare npm, git branch, short hash, bad sha, `ftp://` URL, out-of-bounds timeout, known-field typo; timeout 0 ⇒ default 120. | PASS |
| AC-4 Rollback | `TestWebSearchDisabledPurgesStaleInstall` — disabling purges a previously installed entry + manifest. | PASS |

Manual-only (not a CI gate, per plan §4): a live smoke of a real `pi` session
on Linux ARM64 (Raspberry Pi) and macOS with a real pinned web-search package
(AC-1's platform clause is a real-binary property). To be recorded in the
stage report by the verify phase.

## Local verification (plan "Local verification" — all from repo root)

```
go mod tidy          # no-op: no new external deps (stdlib only)
go test -race ./...  # PASS (all packages)
go vet ./...         # PASS
go build ./...       # PASS (sanity)
```

All commands green. No Makefile exists in the repository, so `make check`
(suggested in the task) does not apply; the three commands above are the
actual `test.yml` CI gate per plan §Local-verification.

## No replan

None of the plan §8 replan triggers fired:
- `pi install` **honors** `PI_CODING_AGENT_DIR` (verified live) — the primary
  install path is usable; the install→copy deviation is the §6 Risk #1
  fallback, not a replan.
- Explicit `-e` loads under `--no-extensions` as documented (no trust gate).
- The extension contract (tool name `web_search`, `PI_AUTOPILOT_WEBSEARCH_*`
  env names) is defined as config constants in `websearch.go` and surfaced by
  the argv/integrity tests; a wrong guess fails those tests immediately.
