# syntax=docker/dockerfile:1

# Build
FROM golang:1.22-alpine AS build
WORKDIR /src
COPY cmd/relay/go.mod cmd/relay/go.sum ./cmd/relay/
WORKDIR /src/cmd/relay
COPY cmd/relay/ .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/linkerdev-relay .

# Runtime
FROM gcr.io/distroless/static:nonroot
USER 65532:65532
COPY --from=build /out/linkerdev-relay /linkerdev-relay
EXPOSE 18080 20000
ENTRYPOINT ["/linkerdev-relay"]
