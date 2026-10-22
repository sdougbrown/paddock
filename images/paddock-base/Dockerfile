FROM docker.io/library/node:22.15-bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        git python3 nftables iproute2 ca-certificates procps \
    && rm -rf /var/lib/apt/lists/*

RUN corepack enable

RUN npm install -g @anthropic-ai/claude-code@2.1.303 \
    && npm cache clean --force
