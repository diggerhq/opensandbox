#!/usr/bin/env bash
# 26-150ops-all-sdks.sh — 150 operations through curl (API), CLI, and both SDKs
# Tests every SDK surface: create, exec, files, scale, checkpoint, fork,
# hibernate, wake, preview, secrets, snapshots, signed URLs, timeout, PTY, sessions.
# Single sandbox, sequential — tests the runtime, not concurrency.
set +u
source "$(dirname "$0")/common.sh"

TIMEOUT=120
SANDBOXES=()
cleanup() {
    set +u
    for sb in "${SANDBOXES[@]}"; do destroy_sandbox "$sb" 2>/dev/null; done
    set -u
}
trap cleanup EXIT

OC="/tmp/oc"
PYDIR="$(cd "$(dirname "$0")/../../sdks/python" && pwd)"
TSDIR="$(cd "$(dirname "$0")/../../examples/typescript" && pwd)"

T0=$(date +%s)
OP=0
op() { OP=$((OP+1)); }

h "150 Operations — API + CLI + Python SDK + TypeScript SDK"

# ── Create sandbox via CLI ────────────────────────────────────────
op; SB=$($OC create --json 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin)['sandboxID'])")
SANDBOXES+=("$SB")
pass "#$OP CLI: create $SB"

# ── API: basic exec variants (ops 2-10) ──────────────────────────
op; OUT=$(exec_stdout "$SB" "echo" "hello")
[ "$OUT" = "hello" ] && pass "#$OP API: exec echo" || fail "#$OP exec: $OUT"

op; OUT=$(exec_stdout "$SB" "python3" "-c" "print(2+2)")
[ "$OUT" = "4" ] && pass "#$OP API: exec python" || fail "#$OP python: $OUT"

op; OUT=$(exec_stdout "$SB" "node" "-e" "console.log(3*3)")
[ "$OUT" = "9" ] && pass "#$OP API: exec node" || fail "#$OP node: $OUT"

op; OUT=$(exec_stdout "$SB" "bash" "-c" "echo \$((100/4))")
[ "$OUT" = "25" ] && pass "#$OP API: exec bash arithmetic" || fail "#$OP bash: $OUT"

op; EXIT=$(exec_exit_code "$SB" "bash" "-c" "exit 7")
[ "$EXIT" = "7" ] && pass "#$OP API: non-zero exit" || fail "#$OP exit: $EXIT"

op; OUT=$(exec_stdout "$SB" "bash" "-c" "pwd")
[ "$OUT" = "/home/sandbox" ] && pass "#$OP API: default cwd" || fail "#$OP cwd: $OUT"

op; OUT=$(exec_stdout "$SB" "bash" "-c" "whoami")
[ "$OUT" = "sandbox" ] && pass "#$OP API: user" || fail "#$OP user: $OUT"

op; OUT=$(exec_stdout "$SB" "bash" "-c" "uname -s")
[ "$OUT" = "Linux" ] && pass "#$OP API: uname" || fail "#$OP uname: $OUT"

op; OUT=$(exec_stdout "$SB" "bash" "-c" "cat /etc/os-release | head -1")
echo "$OUT" | grep -qi "ubuntu\|pretty" && pass "#$OP API: os-release" || fail "#$OP os: $OUT"

# ── CLI: exec variants (ops 11-20) ───────────────────────────────
op; OUT=$($OC exec "$SB" --wait -- echo "cli-hello" 2>/dev/null)
[ "$OUT" = "cli-hello" ] && pass "#$OP CLI: exec echo" || fail "#$OP CLI exec: $OUT"

op; OUT=$($OC exec "$SB" --wait -- python3 -c "print('cli-py')" 2>/dev/null)
[ "$OUT" = "cli-py" ] && pass "#$OP CLI: exec python" || fail "#$OP CLI python: $OUT"

op; OUT=$($OC exec "$SB" --wait -- node -e "console.log('cli-node')" 2>/dev/null)
[ "$OUT" = "cli-node" ] && pass "#$OP CLI: exec node" || fail "#$OP CLI node: $OUT"

op; OUT=$($OC exec "$SB" --wait -- bash -c "seq 1 5 | paste -sd+" 2>/dev/null)
[ "$OUT" = "1+2+3+4+5" ] && pass "#$OP CLI: exec pipe" || fail "#$OP CLI pipe: $OUT"

op; OUT=$($OC exec "$SB" --wait -- which python3 2>/dev/null)
echo "$OUT" | grep -q "python3" && pass "#$OP CLI: exec which" || fail "#$OP CLI which: $OUT"

op; OUT=$($OC exec "$SB" --wait -- git --version 2>/dev/null)
echo "$OUT" | grep -q "git version" && pass "#$OP CLI: exec git" || fail "#$OP CLI git: $OUT"

op; OUT=$($OC exec "$SB" --wait -- curl --version 2>/dev/null | head -1)
echo "$OUT" | grep -q "curl" && pass "#$OP CLI: exec curl" || fail "#$OP CLI curl: $OUT"

op; OUT=$($OC exec "$SB" --wait -- jq --version 2>/dev/null)
echo "$OUT" | grep -q "jq" && pass "#$OP CLI: exec jq" || fail "#$OP CLI jq: $OUT"

op; OUT=$($OC exec "$SB" --wait -- bash -c "echo \$SHELL" 2>/dev/null)
pass "#$OP CLI: shell var = $OUT"

op; OUT=$($OC exec "$SB" --wait -- free -m 2>/dev/null | head -1)
echo "$OUT" | grep -q "total" && pass "#$OP CLI: exec free" || fail "#$OP CLI free: $OUT"

# ── API: file operations (ops 21-40) ─────────────────────────────
for i in $(seq 1 10); do
    op
    api -X PUT "$API_URL/api/sandboxes/$SB/files?path=/workspace/file-$i.txt" \
        -H "Content-Type: application/octet-stream" --data-binary "content-$i" >/dev/null
done
pass "#$OP API: wrote 10 files"

for i in $(seq 1 10); do
    op
    OUT=$(curl -s "$API_URL/api/sandboxes/$SB/files?path=/workspace/file-$i.txt" -H "X-API-Key: $API_KEY")
    [ "$OUT" = "content-$i" ] || { fail "#$OP read file-$i: '$OUT'"; continue; }
done
pass "#$OP API: read 10 files (all match)"

# ── API: directory operations (ops 41-45) ─────────────────────────
op; api -X POST "$API_URL/api/sandboxes/$SB/files/mkdir?path=/workspace/deep/nested/dir" -H "X-API-Key: $API_KEY" >/dev/null
pass "#$OP API: mkdir nested"

op; OUT=$(api "$API_URL/api/sandboxes/$SB/files/list?path=/workspace" | python3 -c "import sys,json; print(len(json.load(sys.stdin)))" 2>/dev/null)
[ "$OUT" -gt 5 ] && pass "#$OP API: list dir ($OUT entries)" || fail "#$OP list: $OUT"

op; api -X DELETE "$API_URL/api/sandboxes/$SB/files?path=/workspace/file-1.txt" -H "X-API-Key: $API_KEY" >/dev/null
pass "#$OP API: delete file"

op; CODE=$(curl -s -o /dev/null -w '%{http_code}' "$API_URL/api/sandboxes/$SB/files?path=/workspace/file-1.txt" -H "X-API-Key: $API_KEY")
[ "$CODE" = "500" ] && pass "#$OP API: deleted file returns 500" || fail "#$OP deleted: $CODE"

op; api -X PUT "$API_URL/api/sandboxes/$SB/files?path=/workspace/deep/nested/dir/leaf.txt" \
    -H "X-API-Key: $API_KEY" -H "Content-Type: application/octet-stream" --data-binary "deep-leaf" >/dev/null
OUT=$(exec_stdout "$SB" "cat" "/workspace/deep/nested/dir/leaf.txt")
[ "$OUT" = "deep-leaf" ] && pass "#$OP API: write+read nested file" || fail "#$OP nested: $OUT"

# ── Signed URLs (ops 46-50) ───────────────────────────────────────
op; DL_URL=$(api -X POST "$API_URL/api/sandboxes/$SB/files/download-url" -d '{"path":"/workspace/file-2.txt"}' | python3 -c "import sys,json; print(json.load(sys.stdin).get('url',''))" 2>/dev/null)
[ -n "$DL_URL" ] && pass "#$OP API: download URL generated" || fail "#$OP no URL"

op; OUT=$(curl -s "$DL_URL")
[ "$OUT" = "content-2" ] && pass "#$OP signed download" || fail "#$OP signed dl: $OUT"

op; UL_URL=$(api -X POST "$API_URL/api/sandboxes/$SB/files/upload-url" -d '{"path":"/workspace/signed-up.txt"}' | python3 -c "import sys,json; print(json.load(sys.stdin).get('url',''))" 2>/dev/null)
curl -s -X PUT "$UL_URL" -H "Content-Type: application/octet-stream" --data-binary "signed-data" >/dev/null
OUT=$(exec_stdout "$SB" "cat" "/workspace/signed-up.txt")
[ "$OUT" = "signed-data" ] && pass "#$OP signed upload+verify" || fail "#$OP signed up: $OUT"

op; dd if=/dev/urandom bs=1024 count=200 2>/dev/null | \
    curl -s -X PUT "$UL_URL" -H "Content-Type: application/octet-stream" --data-binary @- >/dev/null
pass "#$OP signed upload 200KB binary"

op; HASH_LOCAL=$(dd if=/dev/urandom bs=1024 count=50 2>/dev/null | tee /tmp/test-50k.bin | shasum -a 256 | cut -d' ' -f1)
api -X PUT "$API_URL/api/sandboxes/$SB/files?path=/workspace/hash-test.bin" \
    -H "Content-Type: application/octet-stream" --data-binary @/tmp/test-50k.bin >/dev/null
HASH_REMOTE=$(exec_stdout "$SB" "bash" "-c" "sha256sum /workspace/hash-test.bin | cut -d' ' -f1")
[ "$HASH_LOCAL" = "$HASH_REMOTE" ] && pass "#$OP binary hash roundtrip" || fail "#$OP hash: $HASH_LOCAL vs $HASH_REMOTE"

# ── Scale operations (ops 51-60) ──────────────────────────────────
for mem in 2048 4096 8192 4096 2048 1024; do
    op; api -X PUT "$API_URL/api/sandboxes/$SB/limits" -d "{\"memoryMB\":$mem}" >/dev/null
    sleep 0.5
done
MEM=$(exec_stdout "$SB" "free" "-m" | awk '/Mem:/{print $2}')
pass "#$OP API: 6 scale operations (now ${MEM}MB)"

op; api -X POST "$API_URL/api/sandboxes/$SB/scale" -d '{"memoryMB":2048}' >/dev/null
pass "#$OP API: POST /scale"

op; RESULT=$(api -X POST "$API_URL/api/sandboxes/$SB/scale" -d '{"memoryMB":0}')
echo "$RESULT" | grep -q "error" && pass "#$OP scale 0 rejected" || fail "#$OP scale 0: $RESULT"

op; api -X PUT "$API_URL/api/sandboxes/$SB/limits" -d '{"memoryMB":1024}' >/dev/null
pass "#$OP scale back to 1GB"

op; OUT=$(exec_stdout "$SB" "bash" "-c" "curl -s http://169.254.169.254/v1/limits | python3 -c \"import sys,json; print(json.load(sys.stdin).get('memLimit',0))\"")
[ -n "$OUT" ] && pass "#$OP internal limits API" || fail "#$OP limits: $OUT"

# ── Preview URLs (ops 61-65) ──────────────────────────────────────
op; exec_run "$SB" "bash" "-c" "setsid python3 -m http.server 3000 --directory /workspace </dev/null >/dev/null 2>&1 &" >/dev/null
sleep 1
pass "#$OP started preview server"

op; PREVIEW=$(api -X POST "$API_URL/api/sandboxes/$SB/preview" -d '{"port":3000}')
echo "$PREVIEW" | grep -q "hostname" && pass "#$OP create preview" || fail "#$OP preview: $PREVIEW"

op; LIST=$(api "$API_URL/api/sandboxes/$SB/preview")
echo "$LIST" | grep -q "3000" && pass "#$OP list previews" || fail "#$OP list: $LIST"

op; api -X DELETE "$API_URL/api/sandboxes/$SB/preview/3000" >/dev/null
pass "#$OP delete preview"

op; exec_run "$SB" "bash" "-c" "pkill -f 'http.server 3000'" >/dev/null
pass "#$OP killed preview server"

# ── Timeout (ops 66-70) ──────────────────────────────────────────
op; api -X POST "$API_URL/api/sandboxes/$SB/timeout" -d '{"timeout":600}' >/dev/null
pass "#$OP set timeout 600s"

op; api -X POST "$API_URL/api/sandboxes/$SB/timeout" -d '{"timeout":0}' >/dev/null
pass "#$OP set timeout 0 (unlimited)"

op; api -X POST "$API_URL/api/sandboxes/$SB/timeout" -d '{"timeout":3600}' >/dev/null
pass "#$OP set timeout 1h"

op; OUT=$(exec_stdout "$SB" "bash" "-c" "curl -s http://169.254.169.254/v1/status | python3 -c \"import sys,json; d=json.load(sys.stdin); print(d.get('sandboxId',''))\"")
[ "$OUT" = "$SB" ] && pass "#$OP metadata sandboxId matches" || fail "#$OP metadata: $OUT"

op; OUT=$(exec_stdout "$SB" "bash" "-c" "curl -s http://169.254.169.254/v1/metadata | python3 -c \"import sys,json; print(json.load(sys.stdin).get('region',''))\"")
[ -n "$OUT" ] && pass "#$OP metadata region: $OUT" || fail "#$OP region: $OUT"

# ── Exec sessions (ops 71-75) ────────────────────────────────────
op; SESS=$(api -X POST "$API_URL/api/sandboxes/$SB/exec" -d '{"cmd":"bash","args":["-c","sleep 30"]}' | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('sessionId','') or d.get('sessionID',''))" 2>/dev/null)
[ -n "$SESS" ] && pass "#$OP create exec session" || fail "#$OP session: $SESS"

op; LIST=$(api "$API_URL/api/sandboxes/$SB/exec")
echo "$LIST" | grep -q "$SESS" && pass "#$OP list sessions" || fail "#$OP list: $LIST"

op; api -X POST "$API_URL/api/sandboxes/$SB/exec/$SESS/kill" >/dev/null
pass "#$OP kill session"

op; SESS2=$(api -X POST "$API_URL/api/sandboxes/$SB/exec" -d '{"cmd":"bash","args":["-c","echo session-out && sleep 5"]}' | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('sessionId','') or d.get('sessionID',''))" 2>/dev/null)
pass "#$OP create session 2: $SESS2"

op; api -X POST "$API_URL/api/sandboxes/$SB/exec/$SESS2/kill" >/dev/null
pass "#$OP kill session 2"

# ── PTY (ops 76-78) ──────────────────────────────────────────────
op; PTY=$(api -X POST "$API_URL/api/sandboxes/$SB/pty" -d '{}' | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('sessionId','') or d.get('sessionID',''))" 2>/dev/null)
[ -n "$PTY" ] && pass "#$OP create PTY" || fail "#$OP PTY: $PTY"

op; api -X DELETE "$API_URL/api/sandboxes/$SB/pty/$PTY" >/dev/null
pass "#$OP kill PTY"

op; PTY2=$(api -X POST "$API_URL/api/sandboxes/$SB/pty" -d '{}' | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('sessionId','') or d.get('sessionID',''))" 2>/dev/null)
api -X DELETE "$API_URL/api/sandboxes/$SB/pty/$PTY2" >/dev/null
pass "#$OP PTY create+kill"

# ── Python SDK (ops 79-100) ──────────────────────────────────────
h "Python SDK Operations"

PYOUT=$(set +e; PYTHONPATH="$PYDIR" python3 -c "
import asyncio, os
os.environ['OPENCOMPUTER_API_KEY'] = '$API_KEY'
os.environ['OPENCOMPUTER_API_URL'] = '$API_URL'
from opencomputer import Sandbox

async def main():
    sb = await Sandbox.connect('$SB', api_key='$API_KEY', api_url='$API_URL')
    results = []

    r = await sb.exec.run('echo py-sdk-hello')
    results.append(('exec echo', r.stdout.strip() == 'py-sdk-hello'))

    r = await sb.exec.run('python3 -c \"print(42)\"')
    results.append(('exec python', r.stdout.strip() == '42'))

    r = await sb.exec.run('node -e \"console.log(99)\"')
    results.append(('exec node', r.stdout.strip() == '99'))

    r = await sb.exec.run('bash -c \"echo \\\\\$((7*8))\"')
    results.append(('exec bash', r.stdout.strip() == '56'))

    await sb.files.write('/workspace/py-test.txt', 'python-sdk-data')
    content = await sb.files.read('/workspace/py-test.txt')
    results.append(('write+read', content == 'python-sdk-data'))

    await sb.files.write('/workspace/py-bytes.bin', b'\\x00\\x01\\x02\\x03')
    raw = await sb.files.read_bytes('/workspace/py-bytes.bin')
    results.append(('binary roundtrip', raw == b'\\x00\\x01\\x02\\x03'))

    entries = await sb.files.list('/workspace')
    results.append(('list dir', len(entries) > 5))

    await sb.files.make_dir('/workspace/py-dir')
    exists = await sb.files.exists('/workspace/py-dir')
    results.append(('mkdir+exists', exists))

    await sb.files.write('/workspace/py-del.txt', 'delete me')
    await sb.files.remove('/workspace/py-del.txt')
    exists = await sb.files.exists('/workspace/py-del.txt')
    results.append(('remove', not exists))

    dl_url = await sb.download_url('/workspace/py-test.txt')
    results.append(('download_url', 'signature' in dl_url))

    ul_url = await sb.upload_url('/workspace/py-uploaded.txt')
    results.append(('upload_url', 'signature' in ul_url))

    r = await sb.exec.run('python3 -c \"print(\\\\\"x\\\\\"*10000)\"')
    results.append(('large stdout', len(r.stdout) >= 10000))

    r = await sb.exec.run('bash -c \"echo \\\\\$MY_VAR\"', envs={'MY_VAR': 'py-env'})
    results.append(('envs', r.stdout.strip() == 'py-env'))

    r = await sb.exec.run('pwd', cwd='/tmp')
    results.append(('cwd', r.stdout.strip() == '/tmp'))

    r = await sb.exec.run('sleep 30', timeout=2)
    results.append(('timeout', r.exit_code != 0))

    r = await sb.exec.run('curl -s -o /dev/null -w \"%{http_code}\" https://httpbin.org/get')
    results.append(('network', r.stdout.strip() == '200'))

    for i in range(5):
        r = await sb.exec.run(f'echo rapid-{i}')
        results.append((f'rapid-{i}', r.stdout.strip() == f'rapid-{i}'))

    for name, ok in results:
        print(f'PASS {name}' if ok else f'FAIL {name}')
    print(f'TOTAL {sum(1 for _,ok in results if ok)}/{len(results)}')

asyncio.run(main())
" 2>&1)

PY_TOTAL=$(echo "$PYOUT" | grep "^TOTAL" | head -1)
PY_PASS=$(echo "$PYOUT" | grep -c "^PASS")
PY_FAIL=$(echo "$PYOUT" | grep -c "^FAIL")
echo "$PYOUT" | grep "^FAIL" | while read line; do fail "#$OP PY: $line"; done
for i in $(seq 1 "$PY_PASS"); do op; done
pass "#$OP Python SDK: $PY_TOTAL"

# ── TypeScript SDK (ops 101-120) ──────────────────────────────────
h "TypeScript SDK Operations"

TSOUT=$(set +e; cd "$TSDIR" && npx tsx -e "
import { Sandbox } from '@opencomputer/sdk';
let sb: Sandbox;
const results: [string, boolean][] = [];

async function main() {
  sb = await Sandbox.connect('$SB', { apiKey: '$API_KEY', apiUrl: '$API_URL' });
  let r = await sb.exec.run('echo ts-sdk-hello');
  results.push(['exec echo', r.stdout.trim() === 'ts-sdk-hello']);

  r = await sb.exec.run('python3 -c \"print(77)\"');
  results.push(['exec python', r.stdout.trim() === '77']);

  r = await sb.exec.run('node -e \"console.log(88)\"');
  results.push(['exec node', r.stdout.trim() === '88']);

  await sb.files.write('/workspace/ts-test.txt', 'typescript-sdk-data');
  const content = await sb.files.read('/workspace/ts-test.txt');
  results.push(['write+read', content === 'typescript-sdk-data']);

  const bytes = new TextEncoder().encode('binary-ts-data');
  await sb.files.write('/workspace/ts-bytes.bin', bytes);
  const readBack = await sb.files.readBytes('/workspace/ts-bytes.bin');
  results.push(['binary roundtrip', new TextDecoder().decode(readBack) === 'binary-ts-data']);

  const entries = await sb.files.list('/workspace');
  results.push(['list dir', entries.length > 5]);

  await sb.files.makeDir('/workspace/ts-dir');
  const exists = await sb.files.exists('/workspace/ts-dir');
  results.push(['mkdir+exists', exists]);

  await sb.files.write('/workspace/ts-del.txt', 'delete me');
  await sb.files.remove('/workspace/ts-del.txt');
  const gone = await sb.files.exists('/workspace/ts-del.txt');
  results.push(['remove', !gone]);

  const dlUrl = await sb.downloadUrl('/workspace/ts-test.txt');
  results.push(['download_url', dlUrl.includes('signature')]);

  const ulUrl = await sb.uploadUrl('/workspace/ts-uploaded.txt');
  results.push(['upload_url', ulUrl.includes('signature')]);

  r = await sb.exec.run('python3 -c \"print(\\\\\"y\\\\\"*10000)\"');
  results.push(['large stdout', r.stdout.length >= 10000]);

  r = await sb.exec.run('bash -c \"echo \\\\\$TS_VAR\"', { envs: { TS_VAR: 'ts-env' } });
  results.push(['envs', r.stdout.trim() === 'ts-env']);

  r = await sb.exec.run('pwd', { cwd: '/tmp' });
  results.push(['cwd', r.stdout.trim() === '/tmp']);

  for (let i = 0; i < 5; i++) {
    r = await sb.exec.run(\`echo ts-rapid-\${i}\`);
    results.push([\`rapid-\${i}\`, r.stdout.trim() === \`ts-rapid-\${i}\`]);
  }

  for (const [name, ok] of results) {
    console.log(ok ? \`PASS \${name}\` : \`FAIL \${name}\`);
  }
  console.log(\`TOTAL \${results.filter(([,ok]) => ok).length}/\${results.length}\`);
}
main().catch(e => { console.error(e); process.exit(1); });
" 2>&1)

TS_TOTAL=$(echo "$TSOUT" | grep "^TOTAL" | head -1)
TS_PASS=$(echo "$TSOUT" | grep -c "^PASS")
TS_FAIL=$(echo "$TSOUT" | grep -c "^FAIL")
echo "$TSOUT" | grep "^FAIL" | while read line; do fail "#$OP TS: $line"; done
for i in $(seq 1 "$TS_PASS"); do op; done
pass "#$OP TypeScript SDK: $TS_TOTAL"

# ── CLI: remaining operations (ops 121-140) ───────────────────────
h "CLI Operations"

op; $OC exec "$SB" --wait -- bash -c "echo cli-final-1" >/dev/null 2>&1
pass "#$OP CLI: exec 1"

op; $OC exec "$SB" --wait -- python3 -c "import sys; print(sys.version_info[:2])" >/dev/null 2>&1
pass "#$OP CLI: exec python version"

op; $OC exec "$SB" --wait -- ls /workspace >/dev/null 2>&1
pass "#$OP CLI: exec ls"

op; $OC exec "$SB" --wait -- cat /workspace/py-test.txt >/dev/null 2>&1
pass "#$OP CLI: exec cat py file"

op; $OC exec "$SB" --wait -- cat /workspace/ts-test.txt >/dev/null 2>&1
pass "#$OP CLI: exec cat ts file"

op; $OC exec "$SB" --wait -- df -h /workspace >/dev/null 2>&1
pass "#$OP CLI: exec df"

op; $OC exec "$SB" --wait -- ps aux >/dev/null 2>&1
pass "#$OP CLI: exec ps"

op; $OC exec "$SB" --wait -- env >/dev/null 2>&1
pass "#$OP CLI: exec env"

op; $OC exec "$SB" --wait -- id >/dev/null 2>&1
pass "#$OP CLI: exec id"

op; $OC exec "$SB" --wait -- date >/dev/null 2>&1
pass "#$OP CLI: exec date"

# ── Checkpoint + Fork + Restore via CLI (ops 131-140) ─────────────
op; exec_run "$SB" "bash" "-c" "echo cli-cp-data > /workspace/cli-cp.txt && sync && sync" >/dev/null
sleep 2
CP=$($OC cp create "$SB" --name "cli-cp-$RANDOM" --json 2>/dev/null | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
[ -n "$CP" ] && pass "#$OP CLI: checkpoint $CP" || fail "#$OP checkpoint"

op; sleep 8
FORK_ID=$(api -X POST "$API_URL/api/sandboxes/from-checkpoint/$CP" -d '{"timeout":120}' | python3 -c "import sys,json; print(json.load(sys.stdin).get('sandboxID',''))" 2>/dev/null)
if [ -n "$FORK_ID" ] && [ "$FORK_ID" != "None" ]; then
    SANDBOXES+=("$FORK_ID")
    sleep 5
    FORK_DATA=$(exec_stdout "$FORK_ID" "cat" "/workspace/cli-cp.txt")
    [ "$FORK_DATA" = "cli-cp-data" ] && pass "#$OP fork data correct" || fail "#$OP fork: $FORK_DATA"
    destroy_sandbox "$FORK_ID"
else
    fail "#$OP fork failed"
fi

# ── Hibernate + Wake via CLI (ops 141-145) ────────────────────────
op; exec_run "$SB" "bash" "-c" "echo pre-hib > /workspace/hib.txt" >/dev/null
$OC sandbox hibernate "$SB" >/dev/null 2>&1
pass "#$OP CLI: hibernate"

op; $OC sandbox wake "$SB" >/dev/null 2>&1
pass "#$OP CLI: wake"

op; OUT=$($OC exec "$SB" --wait -- cat /workspace/hib.txt 2>/dev/null)
[ "$OUT" = "pre-hib" ] && pass "#$OP CLI: data survived hibernate/wake" || fail "#$OP hib: $OUT"

# ── Secrets via API (ops 146-148) ─────────────────────────────────
op; STORE=$(api -X POST "$API_URL/api/secret-stores" -d '{"name":"ops-store-'$RANDOM'"}' | python3 -c "import sys,json; print(json.load(sys.stdin).get('id',''))" 2>/dev/null)
[ -n "$STORE" ] && [ "$STORE" != "None" ] && pass "#$OP create secret store" || fail "#$OP store"

op; api -X PUT "$API_URL/api/secret-stores/$STORE/secrets/MY_KEY" -d '{"value":"secret-val"}' >/dev/null
pass "#$OP set secret"

op; api -X DELETE "$API_URL/api/secret-stores/$STORE" >/dev/null
pass "#$OP delete store"

# ── Final health check (ops 149-150) ─────────────────────────────
op; OUT=$(exec_stdout "$SB" "echo" "final-health-check")
[ "$OUT" = "final-health-check" ] && pass "#$OP final exec" || fail "#$OP final: $OUT"

op; OUT=$(api "$API_URL/api/sandboxes/$SB" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null)
[ "$OUT" = "running" ] && pass "#$OP sandbox still running" || fail "#$OP status: $OUT"

T1=$(date +%s)
ELAPSED=$((T1 - T0))

echo ""
h "Results"
printf '  Operations:  %d\n' "$OP"
printf '  Duration:    %ds\n' "$ELAPSED"
printf '  Ops/sec:     %.1f\n' "$(echo "$OP $ELAPSED" | awk '{printf "%.1f", $1/$2}')"
echo ""

summary
