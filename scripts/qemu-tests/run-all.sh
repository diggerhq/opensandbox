#!/usr/bin/env bash
# run-all.sh — Run all QEMU backend tests in sequence
#
# Usage:
#   ./scripts/qemu-tests/run-all.sh              # run all tests
#   ./scripts/qemu-tests/run-all.sh 01 03 05     # run specific tests
#   ./scripts/qemu-tests/run-all.sh 05            # run just hibernate tests
#
# Environment (required):
#   OPENSANDBOX_API_URL=http://your-server:8080
#   OPENSANDBOX_API_KEY=your-api-key
#   WORKER_HOST=your-worker-ip

set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

ALL_TESTS=(
    "01-lifecycle"
    "02-exec-files"
    "03-scaling"
    "04-metadata"
    "05-hibernate-wake"
    "06-checkpoint-fork"
    "07-isolation"
    "08-fork-bomb"
    "09-concurrent"
    "10-edge-cases"
    "11-secrets"
    "12-templates"
    "13-preview-urls"
    "14-scale-timeout"
    "15-snapshots-crud"
    "16-exec-sessions"
    "17-pty-sessions"
    "18-restore-checkpoint"
)

# Filter tests if args provided
if [ $# -gt 0 ]; then
    TESTS=()
    for arg in "$@"; do
        for t in "${ALL_TESTS[@]}"; do
            if [[ "$t" == "$arg"* ]]; then
                TESTS+=("$t")
            fi
        done
    done
else
    TESTS=("${ALL_TESTS[@]}")
fi

TOTAL_PASS=0
TOTAL_FAIL=0
FAILED_SUITES=()

echo ""
echo "╔══════════════════════════════════════════════╗"
echo "║   OpenSandbox QEMU Backend Test Suite        ║"
echo "║   ${#TESTS[@]} test suites                              ║"
echo "╚══════════════════════════════════════════════╝"
echo ""

for test in "${TESTS[@]}"; do
    SCRIPT="$SCRIPT_DIR/${test}.sh"
    if [ ! -f "$SCRIPT" ]; then
        echo "  ⊘ $test: script not found"
        continue
    fi

    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"
    echo "  Running: $test"
    echo "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

    if bash "$SCRIPT"; then
        echo ""
    else
        FAILED_SUITES+=("$test")
        echo ""
    fi
done

echo ""
echo "╔══════════════════════════════════════════════╗"
echo "║   Results                                    ║"
echo "╚══════════════════════════════════════════════╝"
echo ""

if [ ${#FAILED_SUITES[@]} -eq 0 ]; then
    echo "  ✓ All ${#TESTS[@]} test suites passed"
    exit 0
else
    echo "  ${#FAILED_SUITES[@]} suite(s) had failures:"
    for suite in "${FAILED_SUITES[@]}"; do
        echo "    ✗ $suite"
    done
    exit 1
fi
