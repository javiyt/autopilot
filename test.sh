#!/bin/bash
# Test script for Pi Autopilot manual mode
set -e

CONFIG_FILE="${1:-config.yaml}"
STATE_DIR=$(dirname "$CONFIG_FILE")/autopilot-state
STATE_FILE="$STATE_DIR/state.json"
MANUAL_CONFIG="$STATE_DIR/manual-config.json"

GREEN='\033[0;32m'; YELLOW='\033[1;33m'; RED='\033[0;31m'; NC='\033[0m'
print_status() { echo -e "${GREEN}[STATUS]${NC} $1"; }
print_info() { echo -e "${YELLOW}[INFO]${NC} $1"; }
print_success() { echo -e "${GREEN}[SUCCESS]${NC} $1"; }
print_error() { echo -e "${RED}[ERROR]${NC} $1"; }

simulate_command() {
    local cmd="$1"; shift
    print_info "Simulating Telegram: /$cmd ${*} (userID: ${USER_ID:-987654321})"
    case "$cmd" in
        begin)
            [ "$#" -lt 1 ] && { print_error "Missing repo"; return 1; }
            repo="$1"; milestone="${2:-}"
            [ -f "$STATE_FILE" ] && cp "$STATE_FILE" "$STATE_DIR/backup-issue-$(cat $STATE_FILE | grep -o '"number":[0-9]*' | cut -d: -f2).json"
            printf '{"repo":"%s","milestone":"%s"}\n' "$repo" "$milestone" > "$MANUAL_CONFIG"
            printf '{"issue":{"number":0}}\n' > "$STATE_FILE"
            print_success "Manual mode started: repo=$repo, milestone=$milestone"
            ;;
        reset)
            [ ! -f "$MANUAL_CONFIG" ] && { print_error "No manual config"; return 1; }
            rm "$MANUAL_CONFIG"
            python3 - "$CONFIG_FILE" "$STATE_FILE" -c "
import json
with open(config) as f: cfg = json.load(f)
cfg.pop('manual_repo', None); cfg.pop('manual_milestone', None)
with open(config, 'w') as f: json.dump(cfg, f, indent=2)
with open(state) as f: state = json.load(f)
state['phase']='idle'; state['issue']={'number':0}; state['manual_repo']=''; state['manual_milestone']=''
with open(state, 'w') as f: json.dump(state, f, indent=2)"
            print_success "Manual config cleared, back to auto-discover mode"
            ;;
        status)
            print_status "Configuration:"
            grep -E "^(repo|milestone|label):" "$CONFIG_FILE" | sed 's/:/ = /'
            echo ""
            [ -f "$STATE_FILE" ] && python3 - "$STATE_FILE" -c "
import json
with open(state) as f: s = json.load(f)
print('Issue: #{}'.format(s.get('issue', {}).get('number', 'N/A')))
print('Phase: {}'.format(s.get('phase', 'N/A')))
print('Paused: {}'.format(s.get('paused', False)))"
            ;;
        *)
            print_error "Unknown command: $cmd"; return 1
            ;;
    esac
}

case "${1:-help}" in
    begin|reset|status) simulate_command "$@" ;;
    clear-state)
        print_info "Removing state directory: $STATE_DIR"
        rm -rf "$STATE_DIR"
        mkdir -p "$STATE_DIR/worktrees" "$STATE_DIR/artifacts"
        print_success "State directory cleared"
        ;;
    help)
        echo "Pi Autopilot Manual Mode Test Script"
        echo "Usage: $0 [begin|reset|status|clear-state|help] [args]"
        echo ""
        echo "Examples: $0 begin world-dolphins/test main"
        echo "          $0 reset; $0 status"
        ;;
    *) print_error "Unknown command: $1"; exit 1 ;;
esac
