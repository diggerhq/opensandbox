# Dockerfile for Claude Chat with OpenSandbox
# Build from repo root: docker build -f examples/claude-chat/Dockerfile -t claude-chat .

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

# Install Claude Code CLI
RUN npm install -g @anthropic-ai/claude-code

# Copy and install OpenSandbox SDK
COPY sdk/python /app/opensandbox-sdk
RUN pip install --no-cache-dir /app/opensandbox-sdk

# Copy and install chat app requirements
COPY examples/claude-chat/requirements.txt /app/requirements.txt
RUN pip install --no-cache-dir -r requirements.txt

# Copy chat application
COPY examples/claude-chat/app.py /app/
COPY examples/claude-chat/static /app/static/

ENV PYTHONUNBUFFERED=1

EXPOSE 8000

CMD ["python", "app.py"]
