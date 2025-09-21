# syntax=docker/dockerfile:1

# Use Docker's automatic build arguments to determine target platform
ARG TARGETOS=linux
ARG TARGETARCH=amd64

# Binary path prefix (set by build process)
ARG BINARY_PREFIX=bin/linkerdev-relay

# Dynamically construct the binary path based on target platform
ARG BINARY_PATH=${BINARY_PREFIX}-${TARGETOS}-${TARGETARCH}

# Runtime
FROM gcr.io/distroless/static:nonroot
ARG BINARY_PATH
ARG TARGETOS
ARG TARGETARCH
LABEL org.opencontainers.image.source="https://github.com/sopatech/linkerdev"
LABEL org.opencontainers.image.architecture=${TARGETARCH}
LABEL org.opencontainers.image.os=${TARGETOS}
USER 65532:65532
COPY ${BINARY_PATH} /linkerdev-relay
EXPOSE 18080 20000
ENTRYPOINT ["/linkerdev-relay"]
