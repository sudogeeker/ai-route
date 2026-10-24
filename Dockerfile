FROM alpine:3

# Install iproute2 (for 'ip' command) and ca-certificates (for HTTPS)
RUN apk add --no-cache iproute2 ca-certificates

# GoReleaser stages a prebuilt binary in each platform directory.
ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/ai-route /usr/bin/ai-route

# Optional gateway auth for /opencode/v1 and OpenCode-backed /v1 routes.
ENV OPENCODE_AUTH_KEY=""
# Optional direct Cloudflare WARP egress for OpenCode.
ENV OPENCODE_WARP="false"
ENV OPENCODE_WARP_POOL_SIZE="2"
ENV OPENCODE_WARP_STACK="ipv4"
# Provider endpoints use the program's built-in defaults.
ENTRYPOINT ["/usr/bin/ai-route"]
