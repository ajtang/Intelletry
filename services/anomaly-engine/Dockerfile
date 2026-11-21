# syntax=docker/dockerfile:1.7
# Build from the repository root: docker build -f services/anomaly-engine/Dockerfile .

FROM --platform=$BUILDPLATFORM golang:1.27.4-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY gen gen
COPY internal internal
COPY services/anomaly-engine services/anomaly-engine
ARG TARGETOS
ARG TARGETARCH
RUN --mount=type=cache,target=/go/pkg/mod --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/app ./services/anomaly-engine

# Distroless has no shell or package manager, which leaves little to exploit, and runs as
# a non-root user by default.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/app"]
