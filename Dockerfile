FROM rust:1.83-slim-bookworm AS builder

WORKDIR /app
COPY Cargo.toml ./
COPY src ./src

# Build without Cargo.lock to avoid version mismatch
RUN cargo build --release

FROM debian:bookworm-slim

# Install dependencies including git and gh CLI
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    procps \
    git \
    curl \
    && rm -rf /var/lib/apt/lists/*

# Install GitHub CLI
RUN curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && chmod go+r /usr/share/keyrings/githubcli-archive-keyring.gpg \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" > /etc/apt/sources.list.d/github-cli.list \
    && apt-get update \
    && apt-get install -y gh \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /app/target/release/isolate /usr/local/bin/isolate

EXPOSE 8080

# Default to server mode
ENTRYPOINT ["/usr/local/bin/isolate"]
CMD ["serve", "--port", "8080"]
