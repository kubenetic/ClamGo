# Multi-stage build for ClamGo
# Stage 1: Build
FROM golang:1.25-alpine AS builder

# Build arguments for version injection (calculated externally)
ARG VERSION=dev
ARG BUILD_TIME=unknown

# Install build dependencies
RUN apk add --no-cache ca-certificates tzdata

# Set working directory
WORKDIR /build

# Copy go mod files
COPY go.mod go.sum ./

# Download dependencies
RUN go mod download

# Copy source code
COPY . .

# Build the application
RUN CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-X main.Version=${VERSION} -X main.BuildTime=${BUILD_TIME} -s -w" \
    -o clamgo \
    .

# Stage 2: Runtime
FROM alpine:latest

# Install runtime dependencies
RUN apk add --no-cache \
    ca-certificates \
    tzdata \
    curl \
    dumb-init

# Create app user
RUN addgroup -g 1000 clamgo && adduser -D -u 1000 -G clamgo clamgo

# Set working directory
WORKDIR /app

# Copy binary from builder
COPY --from=builder /build/clamgo .

# Copy config files
COPY --chown=clamgo:clamgo configs/ ./configs/

# Create necessary directories with proper permissions
RUN mkdir -p /app/data && chown -R clamgo:clamgo /app

# Switch to non-root user
USER clamgo

# Expose health check port
EXPOSE 8080

# Health check
HEALTHCHECK --interval=30s --timeout=10s --start-period=40s --retries=3 \
    CMD curl -f http://localhost:8080/health || exit 1

# Use dumb-init to handle signals properly
ENTRYPOINT ["/sbin/dumb-init", "--"]

# Default command
CMD ["./clamgo"]

# Labels for metadata
LABEL org.opencontainers.image.title="ClamGo" \
      org.opencontainers.image.description="ClamGo - File scanning service with ClamAV" \
      org.opencontainers.image.vendor="ClamGo Project" \
      org.opencontainers.image.documentation="https://github.com/ClamGo/clamgo" \
      org.opencontainers.image.source="https://github.com/ClamGo/clamgo"
