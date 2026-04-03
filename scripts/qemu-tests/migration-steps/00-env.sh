# Source this before running steps
export API_URL="${OPENSANDBOX_API_URL:?Set OPENSANDBOX_API_URL}"
export API_KEY="${OPENSANDBOX_API_KEY:?Set OPENSANDBOX_API_KEY}"

api() {
    curl -s --max-time "${TIMEOUT:-30}" \
        -H "Content-Type: application/json" \
        -H "X-API-Key: $API_KEY" \
        "$@"
}

workers() {
    api "$API_URL/api/workers" | python3 -c "
import sys, json
ws=json.load(sys.stdin)
for w in ws:
    print(f'  {w[\"worker_id\"]:30s} {w[\"current\"]:>3}/{w[\"capacity\"]} mem={w[\"mem_pct\"]:5.1f}%')
print(f'{len(ws)} total')
"
}
