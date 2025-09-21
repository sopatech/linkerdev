# syntax=docker/dockerfile:1

# Accept the pre-built binary path as build argument
ARG BINARY_PATH=bin/linkerdev-relay-linux-amd64

# Runtime
FROM gcr.io/distroless/static:nonroot
USER 65532:65532
COPY ${BINARY_PATH} /linkerdev-relay
EXPOSE 18080 20000
ENTRYPOINT ["/linkerdev-relay"]
