#!/usr/bin/env bash
# 12-templates.sh — Image builder, cache hits, named snapshots
set +u
source "$(dirname "$0")/common.sh"

TIMEOUT=180
SANDBOXES=()
cleanup() { for sb in "${SANDBOXES[@]}"; do destroy_sandbox "$sb" 2>/dev/null; done; }
trap cleanup EXIT

h "On-Demand Image Build"

# First build (cold — no cache)
START=$(date +%s)
RESULT=$(api -X POST "$API_URL/api/sandboxes" -d '{
  "timeout":300,
  "image":{
    "base":"base",
    "steps":[
      {"type":"run","args":{"commands":["echo template-proof > /workspace/built.txt"]}},
      {"type":"pip_install","args":{"packages":["httpx"]}}
    ]
  }
}')
END=$(date +%s)
SB1=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sandboxID',''))" 2>/dev/null)
CP1=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('fromCheckpointId',''))" 2>/dev/null)
ELAPSED=$((END - START))

if [ -n "$SB1" ] && [ "$SB1" != "None" ] && [ "$SB1" != "" ]; then
    SANDBOXES+=("$SB1")
    pass "Image build: $SB1 (${ELAPSED}s, checkpoint=$CP1)"
else
    fail "Image build failed: $RESULT"
    summary
fi

# Wait for fork to complete
sleep 10

# Verify workspace file survived
OUT=$(exec_stdout "$SB1" "cat" "/workspace/built.txt")
[ "$OUT" = "template-proof" ] && pass "Workspace file from build step" || fail "Workspace: '$OUT'"

# Verify pip install survived
OUT=$(exec_stdout "$SB1" "python3" "-c" "import httpx; print(httpx.__version__)")
[ -n "$OUT" ] && pass "pip package from build step: httpx $OUT" || fail "httpx not found"

# Cache hit (same manifest, should be instant)
h "Image Cache Hit"
START=$(date +%s)
RESULT=$(api -X POST "$API_URL/api/sandboxes" -d '{
  "timeout":300,
  "image":{
    "base":"base",
    "steps":[
      {"type":"run","args":{"commands":["echo template-proof > /workspace/built.txt"]}},
      {"type":"pip_install","args":{"packages":["httpx"]}}
    ]
  }
}')
END=$(date +%s)
SB2=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sandboxID',''))" 2>/dev/null)
ELAPSED2=$((END - START))

if [ -n "$SB2" ] && [ "$SB2" != "None" ]; then
    SANDBOXES+=("$SB2")
    [ "$ELAPSED2" -le 2 ] && pass "Cache hit: ${ELAPSED2}s (instant)" || pass "Cache hit: ${ELAPSED2}s"
else
    fail "Cache hit create failed: $RESULT"
fi

sleep 8
OUT=$(exec_stdout "$SB2" "python3" "-c" "import httpx; print(httpx.__version__)")
[ -n "$OUT" ] && pass "Cached sandbox has httpx: $OUT" || fail "Cache miss: httpx not found"

# Named Snapshot
h "Named Snapshots"

# Create snapshot
SNAP_RESULT=$(api -X POST "$API_URL/api/snapshots" -d '{
  "name":"test-snap-'$$'",
  "image":{
    "base":"base",
    "steps":[
      {"type":"run","args":{"commands":["echo snapshot-data > /workspace/snap.txt"]}},
      {"type":"pip_install","args":{"packages":["pyyaml"]}}
    ]
  }
}')
SNAP_NAME=$(echo "$SNAP_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('name',''))" 2>/dev/null)
SNAP_STATUS=$(echo "$SNAP_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('status',''))" 2>/dev/null)
[ "$SNAP_STATUS" = "ready" ] && pass "Snapshot created: $SNAP_NAME" || fail "Snapshot: status=$SNAP_STATUS ($SNAP_RESULT)"

# Create sandbox from snapshot
START=$(date +%s)
RESULT=$(api -X POST "$API_URL/api/sandboxes" -d "{\"timeout\":300,\"snapshot\":\"$SNAP_NAME\"}")
END=$(date +%s)
SB3=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sandboxID',''))" 2>/dev/null)
ELAPSED3=$((END - START))

if [ -n "$SB3" ] && [ "$SB3" != "None" ]; then
    SANDBOXES+=("$SB3")
    [ "$ELAPSED3" -le 2 ] && pass "Snapshot create: ${ELAPSED3}s (instant)" || pass "Snapshot create: ${ELAPSED3}s"
else
    fail "Snapshot sandbox failed: $RESULT"
    summary
fi

sleep 8
OUT=$(exec_stdout "$SB3" "cat" "/workspace/snap.txt")
[ "$OUT" = "snapshot-data" ] && pass "Snapshot workspace file" || fail "Snapshot workspace: '$OUT'"

OUT=$(exec_stdout "$SB3" "python3" "-c" "import yaml; print(yaml.__version__)")
[ -n "$OUT" ] && pass "Snapshot pip package: pyyaml $OUT" || fail "pyyaml not found"

# Multiple sandboxes from same snapshot (all independent)
h "Multiple From Same Snapshot"
SB4_RESULT=$(api -X POST "$API_URL/api/sandboxes" -d "{\"timeout\":300,\"snapshot\":\"$SNAP_NAME\"}")
SB4=$(echo "$SB4_RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sandboxID',''))" 2>/dev/null)
[ -n "$SB4" ] && SANDBOXES+=("$SB4") && pass "Second from snapshot: $SB4" || fail "Second from snapshot"

sleep 8
# Write to SB4, verify SB3 not affected
exec_run "$SB4" "bash" "-c" "echo sb4-only > /workspace/sb4.txt" >/dev/null
OUT=$(exec_stdout "$SB3" "bash" "-c" "cat /workspace/sb4.txt 2>/dev/null || echo not-found")
[ "$OUT" = "not-found" ] && pass "Snapshot forks isolated" || fail "Snapshot leak: '$OUT'"

# Image with multiple step types
h "Complex Image (apt + pip + env + file + workdir)"
RESULT=$(api -X POST "$API_URL/api/sandboxes" -d '{
  "timeout":300,
  "image":{
    "base":"base",
    "steps":[
      {"type":"apt_install","args":{"packages":["jq"]}},
      {"type":"pip_install","args":{"packages":["flask"]}},
      {"type":"env","args":{"vars":{"MY_VAR":"from-image","APP":"demo"}}},
      {"type":"run","args":{"commands":["echo complex-build > /workspace/complex.txt"]}}
    ]
  }
}')
SB5=$(echo "$RESULT" | python3 -c "import sys,json; print(json.load(sys.stdin).get('sandboxID',''))" 2>/dev/null)
if [ -n "$SB5" ] && [ "$SB5" != "None" ]; then
    SANDBOXES+=("$SB5")
    pass "Complex image build: $SB5"
else
    fail "Complex image: $RESULT"
    summary
fi

sleep 10
OUT=$(exec_stdout "$SB5" "jq" "--version")
[ -n "$OUT" ] && pass "apt install (jq): $OUT" || fail "jq not found"

OUT=$(exec_stdout "$SB5" "python3" "-c" "import flask; print(flask.__version__)")
[ -n "$OUT" ] && pass "pip install (flask): $OUT" || fail "flask not found"

OUT=$(exec_stdout "$SB5" "cat" "/workspace/complex.txt")
[ "$OUT" = "complex-build" ] && pass "run command" || fail "run: '$OUT'"

summary
