#!/bin/bash
# Run script for testing OpenSandbox + Claude Agent SDK

set -e

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "$SCRIPT_DIR/../.." && pwd)"

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${GREEN}OpenSandbox + Claude Agent SDK Test${NC}"
echo "========================================"

# Check for .env file
if [ ! -f "$SCRIPT_DIR/.env" ]; then
    echo -e "${YELLOW}Warning: No .env file found. Creating from .env.example${NC}"
    if [ -f "$SCRIPT_DIR/.env.example" ]; then
        cp "$SCRIPT_DIR/.env.example" "$SCRIPT_DIR/.env"
        echo -e "${YELLOW}Please edit .env and add your ANTHROPIC_API_KEY${NC}"
    fi
fi

# Parse arguments
ACTION="${1:-up}"

case "$ACTION" in
    build)
        echo -e "\n${GREEN}Building images...${NC}"
        cd "$SCRIPT_DIR"
        docker compose build
        ;;

    up)
        echo -e "\n${GREEN}Starting services...${NC}"
        cd "$SCRIPT_DIR"
        docker compose up --build
        ;;

    down)
        echo -e "\n${GREEN}Stopping services...${NC}"
        cd "$SCRIPT_DIR"
        docker compose down
        ;;

    test-local)
        echo -e "\n${GREEN}Running tests locally (requires OpenSandbox running)...${NC}"
        cd "$SCRIPT_DIR"

        # Check if ANTHROPIC_API_KEY is set
        if [ -z "$ANTHROPIC_API_KEY" ]; then
            if [ -f .env ]; then
                export $(grep -v '^#' .env | xargs)
            fi
        fi

        # Install dependencies if needed
        pip install -q grpcio grpcio-tools protobuf httpx
        pip install -q -e "$REPO_ROOT/sdk/python" 2>/dev/null || true

        python test_agent.py
        ;;

    build-full)
        echo -e "\n${GREEN}Building full image from repo root...${NC}"
        cd "$REPO_ROOT"
        docker build -f examples/claude-agent-sdk-test/Dockerfile.full -t claude-sandbox-test .
        ;;

    *)
        echo "Usage: $0 {build|up|down|test-local|build-full}"
        echo ""
        echo "  build      - Build Docker images"
        echo "  up         - Start all services with docker compose"
        echo "  down       - Stop all services"
        echo "  test-local - Run tests locally (OpenSandbox must be running)"
        echo "  build-full - Build image from repo root (includes SDK)"
        exit 1
        ;;
esac

echo -e "\n${GREEN}Done!${NC}"
