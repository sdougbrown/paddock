FROM docker.io/library/node:22.15-bookworm-slim

# The node image ships a "node" user at uid 1000, which is the host user's
# uid on most Linux systems; paddock-init must create the agent at that uid,
# so the image gives it up. Nothing in the image runs as that user.
RUN userdel -r node 2>/dev/null || true

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        git python3 nftables iproute2 ca-certificates procps \
    && rm -rf /var/lib/apt/lists/*

RUN corepack enable

RUN npm install -g @anthropic-ai/claude-code@2.1.303 \
    && npm cache clean --force
