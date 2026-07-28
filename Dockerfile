# Build stage
FROM golang:1.26.5-trixie@sha256:4ee9ffa999b4583ce281939cdff828763083610292f252279a0cee77473bd9a7 AS builder

WORKDIR /app

# Copy go mod files first for better caching.
# go.sum is globbed so the build still works before any dependency is added.
COPY go.mod go.sum* ./
RUN go mod download

# Copy source code
COPY . .

# Build arguments for version info
ARG VERSION=dev
ARG COMMIT=unknown
ARG BUILD_TIME=unknown

# Build static binary
RUN CGO_ENABLED=0 go build \
    -trimpath \
    -ldflags="-s -w \
        -X 'github.com/Joibel/triage-bot/internal/buildinfo.Version=${VERSION}' \
        -X 'github.com/Joibel/triage-bot/internal/buildinfo.Commit=${COMMIT}' \
        -X 'github.com/Joibel/triage-bot/internal/buildinfo.BuildTime=${BUILD_TIME}'" \
    -o triage-bot \
    .

# Runtime stage - use distroless for minimal attack surface
FROM gcr.io/distroless/static-debian13:nonroot@sha256:f7f8f729987ad0fdf6b05eeeae94b26e6a0f613bdf46feea7fc40f7bd72953e6

LABEL org.opencontainers.image.title="triage-bot"
LABEL org.opencontainers.image.source="https://github.com/Joibel/triage-bot"

# Copy binary from builder
COPY --from=builder /app/triage-bot /triage-bot

USER nonroot:nonroot

ENTRYPOINT ["/triage-bot"]
