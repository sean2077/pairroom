#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
TMP=$(mktemp -d)
SERVER_PID=""
cleanup() {
  if [[ -n "$SERVER_PID" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill -TERM "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  rm -rf "$TMP"
}
trap cleanup EXIT

REPO="$TMP/repo"
DATA="$TMP/data"
RESTORED="$TMP/restored"
BACKUP="$TMP/room-backup.tar.gz"
BIN=${PAIRROOM_BIN:-$TMP/pairroom}
REPORT=${REPORT:-}
mkdir -p "$REPO"
git -C "$REPO" init -q
git -C "$REPO" config user.name PairRoom-CI
git -C "$REPO" config user.email pairroom-ci@example.invalid
printf 'seed\n' > "$REPO/README.md"
git -C "$REPO" add README.md
git -C "$REPO" commit -qm seed

if [[ ! -x "$BIN" ]]; then
  (cd "$ROOT" && go build -buildvcs=false -trimpath -o "$BIN" ./cmd/pairroom)
fi
PORT=$(python3 - <<'PY'
import socket
s=socket.socket(); s.bind(('127.0.0.1',0)); print(s.getsockname()[1]); s.close()
PY
)
BASE="http://127.0.0.1:$PORT"
"$BIN" serve --repo "$REPO" --data-dir "$DATA" --mock --no-browser \
  --listen "127.0.0.1:$PORT" --routing manual >"$TMP/server.log" 2>&1 &
SERVER_PID=$!

for _ in $(seq 1 100); do
  if curl -fsS "$BASE/api/v1/health" >"$TMP/health.json" 2>/dev/null; then break; fi
  if ! kill -0 "$SERVER_PID" 2>/dev/null; then
    cat "$TMP/server.log" >&2
    exit 1
  fi
  sleep 0.1
done
curl -fsS "$BASE/api/v1/health" >/dev/null

MESSAGE=$(curl -fsS -X POST "$BASE/api/v1/messages" \
  -H 'Content-Type: application/json' \
  --data '{"text":"@all independently review the release boundary and report risks","to":["claude","codex"],"intent":"append"}')
printf '%s' "$MESSAGE" >"$TMP/message.json"

# Exercise the persistent image path and multimodal transcript without relying
# on a vendor network connection.
printf '%s' 'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=' | base64 -d > "$TMP/pixel.png"
curl -fsS -X POST "$BASE/api/v1/attachments" -F "file=@$TMP/pixel.png;type=image/png" > "$TMP/attachment.json"
python3 - "$TMP/attachment.json" "$TMP/image-message.json" <<'PY'
import json,sys
att=json.load(open(sys.argv[1]))
json.dump({'text':'@claude inspect this image','to':['claude'],'attachments':[att]},open(sys.argv[2],'w'))
PY
curl -fsS -X POST "$BASE/api/v1/messages" -H 'Content-Type: application/json' --data-binary "@$TMP/image-message.json" >/dev/null

for _ in $(seq 1 160); do
  curl -fsS "$BASE/api/v1/snapshot?message_limit=250" > "$TMP/snapshot.json"
  if python3 - "$TMP/snapshot.json" <<'PY'
import json,sys
s=json.load(open(sys.argv[1]))
msgs=s.get('messages',[])
participants=s.get('participants',{})
ok=len(msgs)>=5 and all(participants.get(a,{}).get('state') in ('idle','stopped') for a in ('claude','codex'))
raise SystemExit(0 if ok else 1)
PY
  then break; fi
  sleep 0.1
done

python3 - "$TMP/snapshot.json" <<'PY'
import json,sys
s=json.load(open(sys.argv[1]))
assert len(s.get('messages',[]))>=5, s
assert len(s.get('turns',[]))>=3, s.get('turns')
assert any(m.get('attachments') for m in s['messages']), 'attachment missing from transcript'
for actor in ('claude','codex'):
    p=s['participants'][actor]
    assert p['workspace']['kind'] in ('driver-live','reviewer-snapshot'), p
PY

# Cursor API should return a valid page even when the room is still small.
curl -fsS "$BASE/api/v1/messages?limit=2" > "$TMP/page.json"
python3 - "$TMP/page.json" <<'PY'
import json,sys
p=json.load(open(sys.argv[1]))
assert 1 <= len(p['messages']) <= 2, p
assert p['total'] >= len(p['messages']), p
PY

kill -TERM "$SERVER_PID"
wait "$SERVER_PID"
SERVER_PID=""

"$BIN" verify --data-dir "$DATA" --json > "$TMP/verify.json"
"$BIN" backup --data-dir "$DATA" --output "$BACKUP" --json > "$TMP/backup.json"
"$BIN" restore --data-dir "$RESTORED" --input "$BACKUP" --json > "$TMP/restore.json"
"$BIN" verify --data-dir "$RESTORED" --json > "$TMP/verify-restored.json"
"$BIN" diagnostics --data-dir "$DATA" --output "$TMP/diagnostics.tar.gz" >/dev/null

python3 - "$TMP" "$REPORT" <<'PY'
import json,os,sys,hashlib
root,report=sys.argv[1],sys.argv[2]
def load(name): return json.load(open(os.path.join(root,name)))
s=load('snapshot.json'); v=load('verify.json'); r=load('verify-restored.json'); h=load('health.json')
assert v['ok'] and r['ok']
result={
 'pairroom_version':h['version'], 'message_count':len(s['messages']),
 'turn_count':len(s.get('turns',[])), 'latest_seq':s['latest_seq'],
 'attachment_messages':sum(bool(m.get('attachments')) for m in s['messages']),
 'verification_ok':v['ok'], 'restored_verification_ok':r['ok'],
 'event_count':v['event_count'], 'attachment_count':v['attachment_count'],
 'diagnostics_sha256':hashlib.sha256(open(os.path.join(root,'diagnostics.tar.gz'),'rb').read()).hexdigest(),
}
text=json.dumps(result,indent=2,ensure_ascii=False)+'\n'
if report:
 os.makedirs(os.path.dirname(os.path.abspath(report)),exist_ok=True)
 open(report,'w').write(text)
print(text,end='')
PY
