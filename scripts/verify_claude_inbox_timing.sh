#!/usr/bin/env bash
# Manual verification script for Claude inbox turn-boundary behavior
#
# This script helps verify whether Claude Code's cross-session inbox holds
# messages until the current Turn ends (like codex queue) or delivers them
# immediately during a Turn.
#
# Prerequisites:
# - pairroom daemon running
# - Claude Code session bound to a Room
# - jq installed for JSON parsing
#
# Usage: bash scripts/verify_claude_inbox_timing.sh <room-name> <slot-number>

set -euo pipefail

ROOM="${1:-}"
SLOT="${2:-1}"

if [[ -z "$ROOM" ]]; then
  echo "Usage: $0 <room-name> [slot-number]"
  echo ""
  echo "This script verifies Claude inbox message delivery timing."
  echo "It sends messages via the inbox socket while Claude is provably mid-Turn."
  exit 1
fi

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

echo "=== Claude Inbox Turn-Boundary Verification ==="
echo "Room: $ROOM"
echo "Slot: $SLOT"
echo ""

# Get Room status
echo "Step 1: Checking Room status..."
STATUS=$(pairroom relay status --room "$ROOM" --slot "$SLOT" --format json 2>/dev/null || echo "{}")

RUNTIME=$(echo "$STATUS" | jq -r '.runtime // "unknown"')
SESSION_ID=$(echo "$STATUS" | jq -r '.session_id // ""')
BIND_ID=$(echo "$STATUS" | jq -r '.bind_id // ""')

if [[ "$RUNTIME" != "claude" ]]; then
  echo "ERROR: Slot $SLOT runtime is '$RUNTIME', not 'claude'"
  echo "This verification requires a Claude Code session."
  exit 1
fi

if [[ -z "$SESSION_ID" ]]; then
  echo "ERROR: Slot $SLOT is not bound to a session"
  echo "Run 'pairroom relay bind --room $ROOM --slot $SLOT' in the Claude session first."
  exit 1
fi

echo "✓ Claude session bound: $SESSION_ID"
echo "  Bind ID: $BIND_ID"
echo ""

# Check if inbox capability exists
PAIRROOM_ROOT="${PAIRROOM_ROOT:-$HOME/.pairroom}"
INBOX_CAP="$PAIRROOM_ROOT/rooms/$ROOM/slots/slot$SLOT/claude-inbox.json"

if [[ ! -f "$INBOX_CAP" ]]; then
  echo "ERROR: Claude inbox capability file not found: $INBOX_CAP"
  echo "The session may need to run a Stop hook or 'pairroom relay bind' to capture the inbox."
  exit 1
fi

echo "✓ Inbox capability exists"
echo ""

# Parse inbox details
INBOX_ADDRESS=$(jq -r '.address // ""' "$INBOX_CAP")
INBOX_TOKEN=$(jq -r '.token // ""' "$INBOX_CAP")

if [[ -z "$INBOX_ADDRESS" ]] || [[ -z "$INBOX_TOKEN" ]]; then
  echo "ERROR: Invalid inbox capability (missing address or token)"
  exit 1
fi

echo "Inbox socket: $INBOX_ADDRESS"
echo ""

# Helper to send a message via inbox socket
send_inbox_message() {
  local message="$1"
  local temp_script=$(mktemp)

  # Create a Python script to send via the socket
  cat > "$temp_script" <<'PYTHON_EOF'
import json, socket, sys, os

address = sys.argv[1]
token = sys.argv[2]
message = sys.argv[3]

auth_line = json.dumps({"token": token})
message_line = json.dumps({"type": "user", "message": {"role": "user", "content": message}})

try:
    if address.startswith('\\\\.\\pipe\\'):
        # Windows named pipe
        import win32file, win32pipe
        handle = win32file.CreateFile(
            address,
            win32file.GENERIC_READ | win32file.GENERIC_WRITE,
            0, None,
            win32file.OPEN_EXISTING,
            win32file.FILE_FLAG_OVERLAPPED,
            None
        )
        win32file.WriteFile(handle, (auth_line + '\n' + message_line + '\n').encode('utf-8'))
        win32file.CloseHandle(handle)
    else:
        # Unix socket
        sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        sock.connect(address)
        sock.sendall((auth_line + '\n' + message_line + '\n').encode('utf-8'))
        sock.close()
    print("✓ Message sent successfully", file=sys.stderr)
except Exception as e:
    print(f"✗ Failed to send message: {e}", file=sys.stderr)
    sys.exit(1)
PYTHON_EOF

  python3 "$temp_script" "$INBOX_ADDRESS" "$INBOX_TOKEN" "$message" 2>&1
  rm -f "$temp_script"
}

echo "=== Verification Test ==="
echo ""
echo "HYPOTHESIS: Claude inbox holds messages until the Turn ends (like codex queue)"
echo ""
echo "TEST PROCEDURE:"
echo "1. Start a long-running task in the Claude session (e.g., 'sleep 30 seconds then respond')"
echo "2. While Claude is provably mid-Turn, send messages via the inbox socket"
echo "3. Observe when messages become visible to Claude"
echo ""
echo "EXPECTED IF HYPOTHESIS IS CORRECT:"
echo "  - Messages do NOT interrupt the running Turn"
echo "  - Messages become visible only after the Turn ends"
echo ""
echo "EXPECTED IF HYPOTHESIS IS WRONG:"
echo "  - Messages appear immediately in Claude's context during the Turn"
echo ""
echo "---"
echo ""

read -p "Step 2: Start a LONG TASK in Claude now (e.g., 'count to 30 slowly'). Press Enter when task is running... "

echo ""
echo "Step 3: Sending test message #1 via inbox socket..."
send_inbox_message "TEST MESSAGE 1: Sent at $(date +%H:%M:%S) while Claude is mid-Turn"

echo ""
echo "Waiting 5 seconds..."
sleep 5

echo ""
echo "Step 4: Sending test message #2 via inbox socket..."
send_inbox_message "TEST MESSAGE 2: Sent at $(date +%H:%M:%S), still mid-Turn"

echo ""
echo "Waiting 5 seconds..."
sleep 5

echo ""
echo "Step 5: Sending test message #3 via inbox socket..."
send_inbox_message "TEST MESSAGE 3: Sent at $(date +%H:%M:%S), should still be mid-Turn"

echo ""
echo "---"
echo ""
echo "Step 6: MANUAL OBSERVATION REQUIRED"
echo ""
echo "Please observe the Claude session and answer:"
echo ""
echo "Q1: Did any of these test messages appear DURING the long task?"
echo "    (interrupting the Turn)"
echo ""
echo "Q2: When did the messages become visible?"
echo "    a) Immediately as each was sent"
echo "    b) All together when the Turn ended"
echo "    c) Some other pattern"
echo ""
echo "Q3: Did the Claude session show multiple 'PairRoom inbox has messages' notifications?"
echo ""
echo "---"
echo ""
echo "INTERPRETATION:"
echo ""
echo "If messages appeared ONLY AFTER the Turn ended (Q2=b):"
echo "  → Claude inbox behaves like codex queue"
echo "  → SAFE to enable nudge_pending for Claude"
echo "  → This will fix the steering message accumulation issue"
echo ""
echo "If messages appeared DURING the Turn (Q2=a):"
echo "  → Current design is correct (no nudge_pending for Claude)"
echo "  → Need alternative solution (time-based dedup, config option, etc.)"
echo ""
echo "Record your observations in: docs/design/claude-nudge-pending-verification.md"
echo ""
