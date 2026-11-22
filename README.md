# Pi Autopilot — GitHub Delivery workflow supervisor

Go supervisor for managing GitHub delivery workflow via Pi (<https://pi.dev>). Reproduces the ten stages and main handoffs from the [javiyt/pi-github-delivery](https://github.com/javiyt/pi-github-delivery) workflow (reference: `resources/workflow/workflows/GitHubIssueDelivery.yaml`, branch `main`, commit `09c5c2744dabad1a654320da0cb65818fb3f3832`).

## Features

- **Cross-platform**: Runs on Linux, macOS, and Windows with support for multiple model providers
- **Automatic state management**: Manages flow between 10 workflow stages
- **Telegram integration**: Interactive control and real-time diagnostics
- **Cross-platform model support**: Auto-detects appropriate provider based on operating system

## Architecture

Instead of subagents, the supervisor manages state, tools, artifacts, and transitions. Only launches Pi when a stage requires reasoning, code, or independent review. Each call uses `--no-session --no-extensions --no-mcp --no-skills --no-prompt-templates`: clean session, no history from other stages. Input context is **persistent artifacts and the repository**, not prior transcripts.

### Stages and operation

| Original stage | Responsible component |
| --- | --- |
| `repository_setup` | **Go + Git + gh**: clone if missing, validate origin/default, fetch, FF-only, confirm orphaned local branches |
| `investigate` | **Go** collects issue, comments, timeline, PRs, tree, conventions and runs baseline checks; **Pi** analyzes relationships, requirements and code |
| `plan` | **New Pi** with YAML planning model |
| `implement` | **Go** creates/reuses branch; **New Pi** writes code and tests |
| `local_verify` | **Go + shell** execute all `check_commands` with logs and content fingerprint |
| `review` | **New Pi read-only**, YAML reviewer model and decision `APPROVE` / `REQUEST_CHANGES` |
| `publish` | **Go + Git + gh** commit, push, and create/update exactly one PR |
| `ci` | **Go + gh** inspect CI for current SHA; retrieve logs and re-run only demonstrated infrastructure failures |
| `merge` | **Go + gh**, only if merge policy saved at issue start was `auto` and controls remain green |
| `report` | **Go + gh**, verify real status and draft factual report. In manual mode PR remains open |

### Handoffs

- `implement → plan` (invalid plan)
- `local_verify → implement` (checks fail)
- `review → implement` (changes required)
- `ci → implement` (CI fails)

After CI correction, **local_verify, review, publish, and ci** repeat. Attempts are limited and prior reports are preserved.

## Linux Installation

### Requirements

- **System**: Linux (Ubuntu 22.04+, Debian 12+, CentOS 8+, Rocky Linux 9+)
- **Go**: 1.23+ (`go version`)
- **GIT**: `git` in PATH
- **gh**: GitHub CLI authenticated (`gh auth status`)
- **pi**: <https://pi.dev> installed locally
- **Ollama models**: Select models based on your hardware specifications

### Installation

```bash
# Clone or download
unzip pi-autopilot.zip
cd pi-autopilot

# Prepare dependencies
go mod tidy
go test ./...
go build -o pi-autopilot ./cmd/pi-autopilot

# Configure
cp config.example.yaml config.yaml
chmod 600 config.yaml
```

### Ollama Configuration

1. Install Ollama: <https://ollama.com>
2. Download recommended models:

  ```bash
  # For servers with 8GB RAM or more
  ollama pull qwen2.5:7b
  ollama pull qwen2.5:1.5b

  # For servers with 16GB+ RAM
  ollama pull qwen2.5:14b
  ollama pull qwen2.5:32b
  ```

3. Update `config.yaml` with Ollama models:

```yaml
models:
  investigate:
    provider: ollama
    id: qwen2.5:7b
    thinking: low
  plan:
    provider: ollama
    id: qwen2.5:7b
    thinking: medium
  implement:
    provider: ollama
    id: qwen2.5:7b
    thinking: "off"
  review:
    provider: ollama
    id: qwen2.5:1.5b
    thinking: low
  chat:
    provider: ollama
    id: qwen2.5:7b
    thinking: low
```

## macOS Installation

### Requirements

- **System**: macOS (Apple Silicon or Intel)
- **Go**: 1.23+
- **GIT**: `git` in PATH
- **gh**: GitHub CLI authenticated
- **pi**: <https://pi.dev> installed locally
- **MLX**: For Apple Silicon optimized models

### Installation

```bash
unzip pi-autopilot.zip
cd pi-autopilot
go mod tidy
go test ./...
go build -o pi-autopilot ./cmd/pi-autopilot
cp config.example.yaml config.yaml
chmod 600 config.yaml
```

### MLX Model Configuration (Apple Silicon)

Converge MLX models from Hugging Face:

```bash
# Run single model
python -m converge --model-id Qwen/Qwen2.5-7B-Instruct --mlx --quantize 8

# Or use pre-existing MLX models
conda install -c pytorch mlx -c conda-forge conda-forge::mlx
```

Update `config.yaml`:

```yaml
models:
  investigate:
    provider: omlx
    id: Qwen3.5-9B-MLX-8bit
    thinking: low
  plan:
    provider: omlx
    id: Qwen3.8-27B-4bit
    thinking: medium
  implement:
    provider: omlx
    id: Qwen3-Coder-Next-MLX-4bit
    thinking: "off"
  review:
    provider: omlx
    id: GLM-4.7-Flash-MLX-8bit
    thinking: low
  chat:
    provider: omlx
    id: GLM-4.7-Flash-MLX-8bit
    thinking: low
```

### Ollama Configuration (also supported on macOS)

```bash
ollama pull qwen2.5:7b
ollama pull qwen2.5:1.5b
```

```yaml
models:
  investigate:
    provider: ollama
    id: qwen2.5:7b
    thinking: low
  plan:
    provider: ollama
    id: qwen2.5:7b
    thinking: medium
  implement:
    provider: ollama
    id: qwen2.5:7b
    thinking: "off"
  review:
    provider: ollama
    id: qwen2.5:1.5b
    thinking: low
  chat:
    provider: ollama
    id: qwen2.5:7b
    thinking: low
```

## Windows Installation

### Requirements

- **System**: Windows 10/11
- **Go**: 1.23+
- **GIT**: Git for Windows or WSL2
- **gh**: GitHub CLI
- **pi**: <https://pi.dev> (recommended via WSL2 for better performance)

### Installation

```bash
# Option A: Windows Subsystem for Linux 2 (WSL2) - RECOMMENDED
wsl --install
# In the installed Linux distribution:
sudo apt update
sudo apt install git
wget -qO- https://ollama.ai/install.sh | sh
ollama pull qwen2.5:7b
ollama pull qwen2.5:1.5b
```

### Ollama Configuration for Windows

```yaml
models:
  investigate:
    provider: ollama
    id: qwen2.5:7b
    thinking: low
  plan:
    provider: ollama
    id: qwen2.5:7b
    thinking: medium
  implement:
    provider: ollama
    id: qwen2.5:7b
    thinking: "off"
  review:
    provider: ollama
    id: qwen2.5:1.5b
    thinking: low
  chat:
    provider: ollama
    id: qwen2.5:7b
    thinking: low
```

## Complete Model Configuration (cross-platform)

```yaml
# Set the exact IDs returned by: ollama --list (Linux) or pi --list-models (macOS)
models:
  investigate:
    provider: ollama  # or omlx for macOS Apple Silicon
    id: qwen2.5:7b    # change based on your hardware
    thinking: low
  plan:
    provider: ollama
    id: qwen2.5:7b
    thinking: medium
  implement:
    provider: ollama
    id: qwen2.5:7b
    thinking: "off"
  review:
    provider: ollama
    id: qwen2.5:1.5b
    thinking: low
  chat:
    provider: ollama
    id: qwen2.5:7b
    thinking: low

# Models allowed via Telegram commands
allowed_models:
  - qwen2.5:7b
  - qwen2.5:1.5b
```

## Telegram Configuration

For notifications and interactive control, create a bot with BotFather.

1. Chat with @BotFather in Telegram
2. Create a new bot: `/newbot`
3. Copy the token it provides

To get the chat ID:

1. Send `/start` to the bot from your private chat
2. The bot will respond with all detected chat IDs
3. Copy **only your chat ID** to `telegram_chat_id` in the YAML

### Example bot message

```text
Bot: ⛔ Issue #148 blocked in implement due to memory shortage.
You: /models
You: /use implement 3
You: /chat
You: What failed and where can I resume?
Bot: [model diagnostic response, read-only]
You: /fix Split into smaller changes, reuse plan and logs.
Bot: Retrying implement with a new Pi session.
```

### Available commands

`/begin <repo> [milestone]`, `/reset`, `/status`, `/models`, `/use <investigate|plan|implement|review|chat> <n>`, `/chat`, `/ask <question>`, `/exit`, `/fix <prompt>`, `/retry`, `/pause`, `/resume` and `/skip`. The bot only accepts messages from the chosen `telegram_chat_id`.

### Configuration in config.yaml

```yaml
telegram_chat_id: 123456789  # Your chat ID
```

### Manual Mode (Start with Repository Specification)

Pi Autopilot can run in **manual mode** where it waits for a Telegram command to specify the repository and milestone before processing issues. This is useful when you want to control exactly when work begins or when you want to process issues from a specific repository.

**Behavior:**
- If `config.yaml` has `repo: ""` and no issue state is saved, autopilot enters `waiting_for_start` phase
- It polls for Telegram messages but does NOT auto-discover GitHub issues
- Use `/begin <repo> [milestone]` to begin work
- When `/begin` is received, autopilot processes the next issue with that repo/milestone label

**Usage:**

```text
# 1. Start with empty config or manual_repo/milestone in config.yaml
export PI_AUTOPILOT_TELEGRAM_TOKEN='YOUR_BOT_TOKEN'
./pi-autopilot run -config config.yaml

# 2. Send command in Telegram
/begin world-dolphins/test main

# 3. Autopilot will now process issues for that repo/milestone

# 4. To reset to auto-discover mode
/reset
```

**New Commands:**
- `/begin <repo> [milestone]` — Start manual work on specific repo/milestone
- `/reset` — Clear manual config and resume auto-discover mode
- `/status` — View current status (shows "waiting for manual /begin" when in manual mode)

**Example Manual Mode Flow:**

```text
Bot: Pi Autopilot started. Waiting for /begin <repo> [milestone] command.
You: /status
Bot: Autopilot waiting for manual /begin <repo> [milestone] command.
You: /begin world-dolphins/newsletter main
Bot: Starting manual work on repo=world-dolphins/newsletter, milestone=main. The next issue(s) with label="agent-ready" will be processed.
You: /status  
Bot: Issue #1: Fix broken link
    Phase: repository_setup
    Merge policy: manual
    ...
```

## Startup

### Initial preparation

```bash
export PI_AUTOPILOT_TELEGRAM_TOKEN='YOUR_BOT_TOKEN'

# Verify binaries and models
./pi-autopilot doctor -config config.yaml

# Discover chat ID if you don't have it
./pi-autopilot discover-chat

# Verify configuration
./pi-autopilot status
```

### Single run (manual mode)

```bash
./pi-autopilot run -config config.yaml -once
```

Processes one issue and exits. Useful for testing.

### Continuous run (automatic mode)

```bash
./pi-autopilot run -config config.yaml
```

Will continue processing open issues with the `agent-ready` label in ascending order.

### Interactive run with Telegram

```bash
./pi-autopilot run -config config.yaml
# The bot will notify you in Telegram when intervention is needed
```

## Merge policies

- **manual**: PR remains open after green CI, requires human intervention for merge
- **auto**: Supervisor executes `gh pr merge --squash` automatically

The merge policy for an issue remains **immutable once started**. Changing configuration during execution does not affect the merge policy of already-started issues.

## CI and guarantees

- Missing, skipped, pending, or unknown SHA checks are **not green**
- Each review is linked to a SHA-256 fingerprint of changed files relative to base, including untracked files
- Publication is rejected if content no longer matches the fingerprint
- The `merge` stage re-verify PR, head SHA, review status, and CI
- `ci_rerun_patterns` contains known CI infrastructure failure messages
- If logs contain one of the patterns, the supervisor may execute `gh run rerun --failed` **once per run**
- Code errors return to `implement`. Failed checks are not hidden
- Never do `git reset --hard`, `git clean -fd`, force-push, remote branch deletion, or bypass protections
- Local deletions are limited to supervisor **already-merged** branches confirmed with `git branch -d`

## Persistence and resumption

`state_dir/state.json` preserves issue, phase, retries, Telegram temporary models, merge policy, reviewed fingerprint, published head, and CI counter.

Canonical reports live in `state_dir/artifacts/N/`; each successful attempt preserves its own Markdown and immutable handoff within `attempts/<stage>/`. Complete Pi JSONL streams remain in `sessions/` with unique names.

If the supervisor is interrupted during a Pi phase, startup launches a **completely new session**. If a final `handoff.json` for a phase already persisted, it reconciles the transition without repeating it. No coordinating Pi conversation is retained between stages.

## Development and limits

```bash
go test ./...
go test -race ./...
go vet ./...
gofmt -w cmd internal
```

Includes YAML tests, merge policy, fingerprint with untracked files, handoffs, and memory failure recovery tests.

## Cross-platform compatibility

The supervisor auto-detects the platform and suggests the most appropriate model provider:

| Platform | Default provider | Recommended models |
| --- | --- | --- |
| Linux | ollama | qwen2.5:7b, qwen2.5:1.5b |
| macOS (Apple Silicon) | omlx | Qwen3.5-9B-MLX-8bit, Qwen3.8-27B-4bit |
| macOS (Intel) | omlx | Qwen3.5-9B-MLX-8bit, Qwen3.8-27B-4bit |
| Windows | ollama | qwen2.5:7b, qwen2.5:1.5b (via WSL2) |

You can override automatic selection by modifying the `provider` field in `config.yaml`.

## External Provider Support

Pi Autopilot supports multiple model providers for maximum flexibility. You can connect to:

- **Local servers**: Ollama, MLX (macOS Apple Silicon)
- **External servers**: OpenAI API, Anthropic API, compatible endpoints
- **Custom endpoints**: Any OpenAI/Anthropic-compatible API

### Using an external provider

Configure one of these options in `config.yaml`:

#### Option A: External dedicated server

```yaml
models:
  investigate:
    provider: openai
    base_url: https://api.openai.com/v1
    api_key: ""  # or configure OPENAI_API_KEY environment variable
    id: gpt-4o-mini
    thinking: low
```

#### Option B: Ollama server on remote host

```yaml
models:
  investigate:
    provider: ollama
    base_url: http://ollama-server.example.com:11434
    id: qwen2.5:7b
    thinking: low
```

#### Option C: Remote MLX server (macOS)

```yaml
models:
  investigate:
    provider: omlx
    # MLX on remote server requires separate MLX installation
    id: your-mlx-model-id
    thinking: low
```

#### Option D: Custom OpenAI-compatible endpoint

```yaml
models:
  investigate:
    provider: openai-compatible
    base_url: http://llm-custom.example.com/v1
    api_key: ""  # or API_KEY_DEL_SERVIDOR
    id: your-model-name
    thinking: low
```

### Environment variables for API keys

For external providers, use environment variables for API keys:

```bash
OPENAI_API_KEY=sk-... ./pi-autopilot run -config config.yaml
ANTHROPIC_API_KEY=sk-... ./pi-autopilot run -config config.yaml
```

Or configure directly in YAML under `api_key`.

### Supported providers

| Provider | Use case | Base URL examples |
| --- | --- | --- |
| `omlx` | Local Apple Silicon inference | (uses MLX framework) |
| `ollama` | Local or remote Ollama server | `http://localhost:11434` or remote URL |
| `openai` | OpenAI API access | `https://api.openai.com/v1` |
| `anthropic` | Anthropic API access | `https://api.anthropic.com/v1` |
| `openai-compatible` | Any OpenAI-compatible endpoint | Your custom URL |
| `anthropic-compatible` | Any Anthropic-compatible endpoint | Your custom URL |

## Full Configuration Example with External Provider

```yaml
# Repository to manage
repo: OWNER/REPO

# Issue label to trigger autopilot (must exist in repo)
label: agent-ready

# Directory for state and artifacts (relative to config)
state_dir: ./autopilot-state

# Polling interval (seconds) for checking new issues
poll_seconds: 60

# Maximum retries per stage before blocking
max_stage_retries: 3

# Required check commands. All must pass.
check_commands:
  - make check

# ========== MODEL CONFIGURATION ==========
# Using external OpenAI API
models:
  investigate:
    provider: openai
    base_url: https://api.openai.com/v1
    id: gpt-4o-mini
    thinking: low
  plan:
    provider: openai
    base_url: https://api.openai.com/v1
    id: gpt-4o
    thinking: medium
  implement:
    provider: openai
    base_url: https://api.openai.com/v1
    id: gpt-4o
    thinking: "off"
  review:
    provider: openai
    base_url: https://api.openai.com/v1
    id: gpt-4o-mini
    thinking: low
  chat:
    provider: openai
    base_url: https://api.openai.com/v1
    id: gpt-4o-mini
    thinking: low

# Models allowed via Telegram commands
allowed_models:
  - gpt-4o-mini
  - gpt-4o

# Your Telegram bot's authorized chat ID
telegram_chat_id: 0
```

## Startup Commands

### Environment Variables

Create a `.env` file from the example and configure your values:

```bash
cp .env.example .env
# Edit .env with your values
```

**Required:**
- `PI_AUTOPILOT_TELEGRAM_TOKEN`: Your BotFather token
- `PI_AUTOPILOT_AUTHORIZED_USER_ID`: Your Telegram user ID (required for access control)

**Optional:**
- `PI_AUTOPILOT_TELEGRAM_CHAT_ID`: Override chat ID from config.yaml
- Any config.yaml field: `PI_AUTOPILOT_<KEY>` (see below)

### Using Environment Variables in config.yaml

You can embed environment variables in `config.yaml` using `${VAR_NAME}` syntax:

```yaml
# config.yaml
repo: ${PI_AUTOPILOT_REPO:-Mundo-Dolphins/test}  # Uses env or default
label: ${PI_AUTOPILOT_LABEL:-agent-ready}
state_dir: ${PI_AUTOPILOT_STATE_DIR:-./autopilot-state}

# Models with environment variables
models:
  investigate:
    provider: ${PI_AUTOPILOT_PROVIDER:-omlx}
    id: ${PI_AUTOPILOT_MODEL_ID:-Qwen3.5-9B-MLX-8bit}
    thinking: ${PI_AUTOPILOT_MODEL_THINKING:-low}

# External providers with API keys
models:
  implement:
    provider: openai
    base_url: ${PI_AUTOPILOT_OPENAI_BASE_URL:-https://api.openai.com/v1}
    id: ${PI_AUTOPILOT_IMPLEMENT_MODEL:-gpt-4o}
    thinking: "off"
```

**Environment Variable Naming:**
- Format: `PI_AUTOPILOT_<KEY_FROM_YAML>`
- Replace `.` with `_`
- Use uppercase
- Example: For `poll_seconds: 60`, use `PI_AUTOPILOT_POLL_SECONDS=60`

**Default Values:**
- `${VAR}`: Uses env value or empty string
- `${VAR:-default}`: Uses env value or specified default

### Start Autopilot

```bash
# Load environment variables
export $(cat .env | xargs)

# Interactive mode with Telegram notifications
./pi-autopilot run -config config.yaml

# Manual single issue run
./pi-autopilot run -config config.yaml -once

# View status without running
./pi-autopilot status

# Diagnose setup and verify models
./pi-autopilot doctor -config config.yaml
```

## References

- [javiyt/pi-github-delivery](https://github.com/javiyt/pi-github-delivery) - Original workflow
- [pi.dev](https://pi.dev) - Pi reasoning tool
- [ollama.com](https://ollama.com) - Platform for local language models
- [OpenAI API](https://platform.openai.com) - OpenAI API documentation
- [Anthropic API](https://www.anthropic.com/api) - Anthropic API documentation
- [BotFather](https://t.me/BotFather) - Create Telegram bots
