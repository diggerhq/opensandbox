# Dockerfile for testing OpenSandbox + Claude Agent SDK
# This image can be used as:
# 1. A client that connects to OpenSandbox and runs Claude Agent SDK tasks
# 2. A base for the sandbox itself (with Claude Code pre-installed)

FROM python:3.11-slim

WORKDIR /app

# Install system dependencies
RUN apt-get update && apt-get install -y \
    curl \
    git \
    ca-certificates \
    gnupg \
    && rm -rf /var/lib/apt/lists/*

# Install Node.js (required for Claude Code CLI)
RUN curl -fsSL https://deb.nodesource.com/setup_20.x | bash - \
    && apt-get install -y nodejs \
    && rm -rf /var/lib/apt/lists/*

# Install Claude Code CLI globally via npm
# Note: This may fail if the package isn't published yet - that's OK for testing
RUN npm install -g @anthropic-ai/claude-code || echo "Claude Code npm package not yet available"

# Install Python dependencies first (for caching)
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt || \
    pip install --no-cache-dir grpcio grpcio-tools protobuf httpx pytest pytest-asyncio

# Copy test scripts
COPY *.py ./

# Set environment variables
ENV PYTHONUNBUFFERED=1
ENV PYTHONPATH=/app

# Default command - can be overridden
CMD ["python", "test_agent.py"]
