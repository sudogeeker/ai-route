FROM alpine:3

RUN apk add --no-cache ca-certificates

# GoReleaser stages a prebuilt binary in each platform directory.
ARG TARGETPLATFORM
COPY ${TARGETPLATFORM}/ai-route /usr/bin/ai-route

ENTRYPOINT ["/usr/bin/ai-route"]
