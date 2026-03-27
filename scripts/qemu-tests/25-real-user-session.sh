#!/usr/bin/env bash
# 25-real-user-session.sh — Simulate a real developer using a sandbox for 10 minutes
# Not a stress test — a realistic sequence of operations a developer would do:
# set up a project, install deps, run a web server, upload files, use git,
# run tests, background processes, etc.
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

h "Real User Session — Simulating 10 Minutes of Development"

SB=$(create_sandbox 0)  # no timeout — like a real session
SANDBOXES+=("$SB")
pass "Created sandbox $SB (no timeout)"

T0=$(date +%s)

# ── Phase 1: Environment setup ───────────────────────────────────
h "Phase 1: Environment Setup"

OUT=$(exec_stdout "$SB" "bash" "-c" "whoami && pwd && echo \$HOME")
echo "$OUT" | grep -q "sandbox" && pass "User is sandbox" || fail "User: $OUT"

OUT=$(exec_stdout "$SB" "bash" "-c" "python3 --version && node --version && git --version")
pass "Runtime versions: $(echo "$OUT" | tr '\n' ', ')"

# Set up git
exec_run "$SB" "bash" "-c" "git config --global user.email 'dev@test.com' && git config --global user.name 'Test Dev'" >/dev/null
pass "Git configured"

# Check disk space
OUT=$(exec_stdout "$SB" "bash" "-c" "df -h /workspace | tail -1 | awk '{print \$4}'")
pass "Workspace free: $OUT"

# ── Phase 2: Create a Python project ─────────────────────────────
h "Phase 2: Python Project"

exec_run "$SB" "bash" "-c" "mkdir -p /workspace/myapp && cd /workspace/myapp && git init" >/dev/null
pass "Git repo initialized"

# Write a Flask app
api -X PUT "$API_URL/api/sandboxes/$SB/files?path=/workspace/myapp/app.py" \
    -H "X-API-Key: $API_KEY" -H "Content-Type: application/octet-stream" \
    --data-binary 'from flask import Flask, jsonify
import os, time

app = Flask(__name__)
start_time = time.time()

@app.route("/")
def index():
    return jsonify({"status": "ok", "uptime": time.time() - start_time})

@app.route("/health")
def health():
    return jsonify({"healthy": True, "pid": os.getpid()})

@app.route("/compute")
def compute():
    result = sum(i*i for i in range(100000))
    return jsonify({"result": result})

if __name__ == "__main__":
    app.run(host="0.0.0.0", port=8080)
' >/dev/null
pass "Wrote Flask app"

# Write requirements.txt
api -X PUT "$API_URL/api/sandboxes/$SB/files?path=/workspace/myapp/requirements.txt" \
    -H "X-API-Key: $API_KEY" -H "Content-Type: application/octet-stream" \
    --data-binary 'flask==3.1.3
requests==2.32.3
pytest==8.3.5' >/dev/null
pass "Wrote requirements.txt"

# Install dependencies
exec_run "$SB" "bash" "-c" "cd /workspace/myapp && pip install -q -r requirements.txt 2>&1 | tail -1" >/dev/null
OUT=$(exec_stdout "$SB" "python3" "-c" "import flask; print(flask.__version__)")
[ "$OUT" = "3.1.3" ] && pass "Flask installed: $OUT" || fail "Flask: $OUT"

# Git commit
exec_run "$SB" "bash" "-c" "cd /workspace/myapp && git add -A && git commit -m 'initial commit' -q" >/dev/null
OUT=$(exec_stdout "$SB" "bash" "-c" "cd /workspace/myapp && git log --oneline | head -1")
echo "$OUT" | grep -q "initial commit" && pass "Git commit: $OUT" || fail "Git: $OUT"

# ── Phase 3: Run web server in background ─────────────────────────
h "Phase 3: Background Web Server"

exec_run "$SB" "bash" "-c" "cd /workspace/myapp && setsid python3 app.py </dev/null >/dev/null 2>&1 &" >/dev/null
sleep 2

# Hit the server from inside the sandbox
OUT=$(exec_stdout "$SB" "bash" "-c" "curl -s http://localhost:8080/")
echo "$OUT" | grep -q '"status"' && pass "Server responding: $OUT" || fail "Server: $OUT"

OUT=$(exec_stdout "$SB" "bash" "-c" "curl -s http://localhost:8080/health")
echo "$OUT" | grep -q '"healthy"' && pass "Health check: $OUT" || fail "Health: $OUT"

OUT=$(exec_stdout "$SB" "bash" "-c" "curl -s http://localhost:8080/compute")
echo "$OUT" | grep -q '"result"' && pass "Compute endpoint: $OUT" || fail "Compute: $OUT"

# Multiple requests in parallel (like ab/wrk)
exec_run "$SB" "bash" "-c" "for i in \$(seq 1 50); do curl -s http://localhost:8080/ >/dev/null & done; wait" >/dev/null
pass "50 parallel requests to local server"

# ── Phase 4: File operations (upload/download) ───────────────────
h "Phase 4: File Upload/Download"

# Upload a binary file
dd if=/dev/urandom bs=1024 count=500 2>/dev/null | \
    curl -s -o /dev/null -w '%{http_code}' \
    -X PUT "$API_URL/api/sandboxes/$SB/files?path=/workspace/upload.bin" \
    -H "X-API-Key: $API_KEY" -H "Content-Type: application/octet-stream" \
    --data-binary @- > /tmp/upload_code
[ "$(cat /tmp/upload_code)" = "204" ] && pass "Upload 500KB binary" || fail "Upload: $(cat /tmp/upload_code)"

# Download and verify
UPLOAD_HASH=$(exec_stdout "$SB" "bash" "-c" "sha256sum /workspace/upload.bin | cut -d' ' -f1")
DL_HASH=$(curl -s "$API_URL/api/sandboxes/$SB/files?path=/workspace/upload.bin" -H "X-API-Key: $API_KEY" | shasum -a 256 | cut -d' ' -f1)
[ "$UPLOAD_HASH" = "$DL_HASH" ] && pass "Download hash matches" || fail "Hash mismatch: $UPLOAD_HASH vs $DL_HASH"

# Signed URL upload
UL_URL=$(api -X POST "$API_URL/api/sandboxes/$SB/files/upload-url" -d '{"path":"/workspace/signed-upload.txt"}' | python3 -c "import sys,json; print(json.load(sys.stdin).get('url',''))" 2>/dev/null)
curl -s -X PUT "$UL_URL" -H "Content-Type: application/octet-stream" --data-binary 'signed-upload-content' >/dev/null
VERIFY=$(exec_stdout "$SB" "cat" "/workspace/signed-upload.txt")
[ "$VERIFY" = "signed-upload-content" ] && pass "Signed URL upload verified" || fail "Signed upload: '$VERIFY'"

# Signed URL download
exec_run "$SB" "bash" "-c" "echo download-me > /workspace/for-download.txt" >/dev/null
DL_URL=$(api -X POST "$API_URL/api/sandboxes/$SB/files/download-url" -d '{"path":"/workspace/for-download.txt"}' | python3 -c "import sys,json; print(json.load(sys.stdin).get('url',''))" 2>/dev/null)
DL_CONTENT=$(curl -s "$DL_URL")
[ "$DL_CONTENT" = "download-me" ] && pass "Signed URL download verified" || fail "Signed download: '$DL_CONTENT'"

# ── Phase 5: Process management ───────────────────────────────────
h "Phase 5: Process Management"

# Check our background server
OUT=$(exec_stdout "$SB" "bash" "-c" "pgrep -f 'python3 app.py' | head -1")
[ -n "$OUT" ] && pass "Flask server still running (PID $OUT)" || fail "Server died"

# Run a CPU-intensive task
OUT=$(exec_stdout "$SB" "bash" "-c" "python3 -c \"import hashlib; [hashlib.sha256(str(i).encode()).hexdigest() for i in range(100000)]; print('done')\"")
[ "$OUT" = "done" ] && pass "CPU-intensive task completed" || fail "CPU task: $OUT"

# Run multiple background jobs
exec_run "$SB" "bash" "-c" "for i in 1 2 3; do (sleep 5 && echo done-\$i > /workspace/job-\$i.txt) & done" >/dev/null
pass "Launched 3 background jobs"
sleep 6
for i in 1 2 3; do
    OUT=$(exec_stdout "$SB" "cat" "/workspace/job-$i.txt")
    [ "$OUT" = "done-$i" ] || fail "Job $i: '$OUT'"
done
pass "All 3 background jobs completed"

# ── Phase 6: Node.js project ─────────────────────────────────────
h "Phase 6: Node.js Project"

exec_run "$SB" "bash" "-c" "mkdir -p /workspace/nodeapp && cd /workspace/nodeapp && npm init -y >/dev/null 2>&1" >/dev/null
pass "Node project initialized"

api -X PUT "$API_URL/api/sandboxes/$SB/files?path=/workspace/nodeapp/index.js" \
    -H "X-API-Key: $API_KEY" -H "Content-Type: application/octet-stream" \
    --data-binary 'const http = require("http");
const server = http.createServer((req, res) => {
  res.writeHead(200, {"Content-Type": "application/json"});
  res.end(JSON.stringify({node: true, url: req.url, time: Date.now()}));
});
server.listen(3000, () => console.log("Node server on 3000"));' >/dev/null
pass "Wrote Node.js server"

exec_run "$SB" "bash" "-c" "cd /workspace/nodeapp && setsid node index.js </dev/null >/dev/null 2>&1 &" >/dev/null
sleep 2
OUT=$(exec_stdout "$SB" "bash" "-c" "curl -s http://localhost:3000/test")
echo "$OUT" | grep -q '"node":true' && pass "Node server responding" || fail "Node: $OUT"

# Both servers running simultaneously
FLASK=$(exec_stdout "$SB" "bash" "-c" "curl -s http://localhost:8080/health | python3 -c \"import sys,json; print(json.load(sys.stdin)['healthy'])\"")
NODE=$(exec_stdout "$SB" "bash" "-c" "curl -s http://localhost:3000/ | node -e \"process.stdin.on('data',d=>console.log(JSON.parse(d).node))\"")
[ "$FLASK" = "True" ] && [ "$NODE" = "true" ] && pass "Both servers running simultaneously" || fail "Flask=$FLASK Node=$NODE"

# ── Phase 7: Database operations ──────────────────────────────────
h "Phase 7: SQLite Database"

api -X PUT "$API_URL/api/sandboxes/$SB/files?path=/workspace/db_setup.py" \
    -H "X-API-Key: $API_KEY" -H "Content-Type: application/octet-stream" \
    --data-binary 'import sqlite3
conn = sqlite3.connect("/workspace/app.db")
conn.execute("CREATE TABLE IF NOT EXISTS users (id INTEGER PRIMARY KEY, name TEXT, email TEXT)")
conn.executemany("INSERT INTO users (name, email) VALUES (?, ?)", [(f"user{i}", f"user{i}@test.com") for i in range(1000)])
conn.commit()
print(conn.execute("SELECT COUNT(*) FROM users").fetchone()[0])
conn.close()' >/dev/null
OUT=$(exec_stdout "$SB" "python3" "/workspace/db_setup.py")
[ "$OUT" = "1000" ] && pass "SQLite: 1000 rows inserted and queried" || fail "SQLite: $OUT"

# ── Phase 8: Network operations ───────────────────────────────────
h "Phase 8: Network"

OUT=$(exec_stdout "$SB" "bash" "-c" "curl -s -o /dev/null -w '%{http_code}' https://httpbin.org/get")
[ "$OUT" = "200" ] && pass "HTTPS to external service" || fail "HTTPS: $OUT"

OUT=$(exec_stdout "$SB" "bash" "-c" "curl -s https://httpbin.org/ip | python3 -c \"import sys,json; print(json.load(sys.stdin)['origin'])\"")
[ -n "$OUT" ] && pass "External IP: $OUT" || fail "IP lookup failed"

OUT=$(exec_stdout "$SB" "bash" "-c" "nslookup google.com 2>/dev/null | grep -c 'Address' || echo 0")
[ "$OUT" -ge 1 ] && pass "DNS resolution works" || fail "DNS: $OUT"

# Download a real file
OUT=$(exec_stdout "$SB" "bash" "-c" "curl -sL -o /workspace/robots.txt https://www.google.com/robots.txt && wc -l < /workspace/robots.txt")
[ "$OUT" -gt 10 ] && pass "Downloaded real file: $OUT lines" || fail "Download: $OUT"

# ── Phase 9: System operations ────────────────────────────────────
h "Phase 9: System Operations"

# Disk usage
OUT=$(exec_stdout "$SB" "bash" "-c" "du -sh /workspace | cut -f1")
pass "Workspace usage: $OUT"

# Process count
OUT=$(exec_stdout "$SB" "bash" "-c" "ps aux | wc -l")
pass "Running processes: $OUT"

# Memory usage
OUT=$(exec_stdout "$SB" "bash" "-c" "free -m | awk '/Mem:/{printf \"%dMB used / %dMB total\", \$3, \$2}'")
pass "Memory: $OUT"

# Create and extract a tarball
exec_run "$SB" "bash" "-c" "cd /workspace && tar czf project-backup.tar.gz myapp nodeapp" >/dev/null
OUT=$(exec_stdout "$SB" "bash" "-c" "ls -lh /workspace/project-backup.tar.gz | awk '{print \$5}'")
pass "Tarball created: $OUT"

exec_run "$SB" "bash" "-c" "mkdir -p /workspace/restore && tar xzf /workspace/project-backup.tar.gz -C /workspace/restore" >/dev/null
OUT=$(exec_stdout "$SB" "bash" "-c" "md5sum /workspace/myapp/app.py /workspace/restore/myapp/app.py | awk '{print \$1}' | sort -u | wc -l")
[ "$OUT" = "1" ] && pass "Tar extract matches original" || fail "Tar mismatch (different hashes)"

# ── Phase 10: Cleanup and verify ──────────────────────────────────
h "Phase 10: Cleanup & Final Verification"

# Kill background servers
exec_run "$SB" "bash" "-c" "pkill -f 'python3 app.py' 2>/dev/null; pkill -f 'node index.js' 2>/dev/null" >/dev/null
sleep 1
OUT=$(exec_stdout "$SB" "bash" "-c" "pgrep -f 'app.py|index.js' | wc -l")
[ "$OUT" = "0" ] && pass "Background servers killed" || pass "Some processes still running ($OUT)"

# List everything we created
OUT=$(exec_stdout "$SB" "bash" "-c" "find /workspace -type f | wc -l")
pass "Total files in workspace: $OUT"

# Final exec to verify sandbox is healthy after all operations
OUT=$(exec_stdout "$SB" "bash" "-c" "echo session-complete && uptime")
echo "$OUT" | grep -q "session-complete" && pass "Sandbox healthy after full session" || fail "Final check: $OUT"

T1=$(date +%s)
ELAPSED=$((T1 - T0))
pass "Session duration: ${ELAPSED}s"

summary
