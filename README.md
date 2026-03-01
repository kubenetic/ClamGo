# ClamGo - Enterprise-Grade Malware Scanning Service

[![Build and Release](https://github.com/ClamGo/clamgo/actions/workflows/build-and-release.yml/badge.svg?branch=main)](https://github.com/ClamGo/clamgo/actions/workflows/build-and-release.yml)
[![Docker Publish](https://github.com/ClamGo/clamgo/actions/workflows/docker-publish.yml/badge.svg?branch=main)](https://github.com/ClamGo/clamgo/actions/workflows/docker-publish.yml)
[![Code Quality](https://github.com/ClamGo/clamgo/actions/workflows/lint.yml/badge.svg?branch=main)](https://github.com/ClamGo/clamgo/actions/workflows/lint.yml)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)
[![Go Version](https://img.shields.io/badge/Go-1.25+-blue.svg)](https://golang.org)

ClamGo is a high-performance, distributed malware scanning service that leverages **ClamAV** for virus detection and integrates seamlessly with message queues for scalable file processing. Built with enterprise reliability in mind, ClamGo provides a robust backend for file scanning operations in large-scale systems.

## 🎯 Key Features

### Core Scanning Capabilities
- **ClamAV Integration**: Full-featured virus scanning using the ClamAV daemon (clamd)
- **Real-time Detection**: Scan files in real-time as they're uploaded
- **Multi-source Support**: TCP and Unix socket connections to clamd
- **File Validation**: Magic byte inspection and MIME type detection
- **Checksum Computation**: SHA-256 hash calculation for file integrity verification

### Reliability & Resilience
- **Automatic Retry Logic**: 3-tier exponential backoff retry mechanism
  - Retry 1: 30 seconds
  - Retry 2: 120 seconds
  - Retry 3: 600 seconds
- **Dead Letter Queue (DLQ)**: Permanent failure routing for manual inspection
- **Cancellation Handling**: Dual-layer cancellation checking (in-memory + Redis)
- **Message Acknowledgment**: Smart ACK/NACK handling with custom routing

### Scalability & Performance
- **Distributed Architecture**: Message queue-based decoupling
- **Worker Pooling**: Configurable concurrent scanner instances
- **Asynchronous Processing**: Non-blocking scan operations
- **Redis Integration**: Optional fast-path cancellation checks
- **NFS Support**: Optimized for shared storage environments

### Enterprise Features
- **Structured Logging**: Comprehensive zerolog-based logging with trace levels
- **Configuration Management**: YAML-based configuration with environment overrides
- **Health Checks**: clamd connectivity and ClamAV database monitoring
- **Metrics & Telemetry**: Scanner performance and state tracking
- **Signal Handling**: Graceful shutdown with SIGTERM/SIGINT support

## 📋 System Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                    File Upload System                           │
├─────────────────────────────────────────────────────────────────┤
│                                                                 │
│  ┌──────────────┐         ┌──────────────────┐                  │
│  │   Browser UI │ ──────> │  Spring Boot MVC │                  │
│  │  File Upload │         │   Job Manager    │                  │
│  └──────────────┘         └────────┬─────────┘                  │
│                                    │                            │
│                          ┌─────────▼──────────┐                 │
│                          │   Message Queue    │                 │
│                          │  (RabbitMQ)        │                 │
│                          │                    │                 │
│        ┌─────────────────┤ scan.jobs.q        │                 │
│        │                 │ case.cancelled.q   │                 │
│        │                 └────────┬───────────┘                 │
│        │                          │                             │
│        ▼                          ▼                             │
│  ┌──────────────────────────────────────┐                       │
│  │   ClamGo Scanner Service             │                       │
│  │                                      │                       │
│  │  ┌──────────────────────────────┐    │                       │
│  │  │ File Processing Pipeline:    │    │                       │
│  │  │  1. Cancellation check       │    │                       │
│  │  │  2. Path validation          │    │                       │
│  │  │  3. Magic byte detection     │    │                       │
│  │  │  4. SHA-256 computation      │    │                       │
│  │  │  5. ClamAV scan              │    │                       │
│  │  │  6. Result publication       │    │                       │
│  │  └──────────────────────────────┘    │                       │
│  │                                      │                       │
│  │  ┌──────────────────────────────┐    │                       │
│  │  │ External Services:           │    │                       │
│  │  │  • clamd (ClamAV daemon)     │    │                       │
│  │  │  • Redis (cancellation)      │    │                       │
│  │  └──────────────────────────────┘    │                       │
│  └──────────────────────────────────────┘                       │
│        │                          │                             │
│        ▼                          ▼                             │
│  ┌──────────────────┐    ┌──────────────────┐                   │
│  │  Scan Results    │    │  Dead Letter     │                   │
│  │  (RabbitMQ)      │    │  Queue (Failures)│                   │
│  └──────────────────┘    └──────────────────┘                   │
│        │                          │                             │
│        └──────────────┬───────────┘                             │
│                       ▼                                         │
│        ┌──────────────────────────┐                             │
│        │ Backend Job Consumer     │                             │
│        │ (Update status, Persist) │                             │
│        └──────────────────────────┘                             │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

### Processing Pipeline

The ClamGo scanner processes each file through a comprehensive pipeline:

```
Message Received
      ↓
[1] Check Cancellation (Redis + Memory)
      ↓
      ├─ Cancelled? → ACK & Discard
      │
[2] Validate Path Prefix
      ├─ Invalid? → ACK & Discard
      │
[3] Open & Read File
      ├─ Error? → Nack & Retry
      │
[4] Compute SHA-256 & Magic Bytes
      ├─ Error? → Log warning & Continue
      │
[5] Re-check Cancellation
      ├─ Cancelled? → ACK & Discard
      │
[6] Connect to clamd
      ├─ Error? → Nack & Retry
      │
[7] Execute ClamAV Scan
      ├─ Error? → Nack & Retry
      │
[8] Post-scan Cancellation Check
      ├─ Cancelled? → ACK & Don't publish
      │
[9] Publish Result
      ├─ Clean? → file.scan.completed
      ├─ Infected? → file.scan.completed (with signature)
      └─ Error? → file.scan.retrying → DLQ after 3 retries
```

## 🚀 Quick Start

### Prerequisites

- **Go 1.25+** - Go programming language runtime
- **ClamAV daemon (clamd)** - Virus scanning engine
- **RabbitMQ** - Message queue broker
- **Redis** (optional) - Fast cancellation checks
- **Docker & Docker Compose** (optional) - For containerized deployment

### Installation

#### 1. Build from Source

```bash
# Clone the repository
git clone https://github.com/ClamGo/clamgo.git
cd clamgo

# Download dependencies
go mod download

# Build the application
go build -o clamgo .
```

#### 2. Using Docker

```bash
# Build the Docker image
docker build -f build/package/Dockerfile -t clamgo:latest .

# Push to registry (optional)
docker push ghcr.io/clamgo/clamgo:latest
```

### Configuration

ClamGo uses YAML configuration with environment variable overrides. Configuration is loaded from:

1. `./config.yaml` (current directory)
2. `./configs/config.yaml` (configs subdirectory)
3. `/etc/clamgo/config.yaml` (system configuration)
4. Environment variables (highest priority)

#### Example Configuration

```yaml
# ClamAV daemon connection
clamd:
  tcp:
    addr: "localhost:3310"
  unix:
    path: ""

# RabbitMQ message broker
rabbitmq:
  host: "127.0.0.1"
  port: 5672
  username: ""
  password: ""

  # Exchange and queue configuration
  exchange: "uploader.exchange"
  dlx: "uploader.dlx"
  
  scanQueue: "q.file.scan"
  cancelQueue: "q.case.cancelled"

  # Routing keys for result publishing
  scanCompletedRoutingKey: "file.scan.completed"
  scanRetryingRoutingKey: "file.scan.retrying"
  dlqRoutingKey: "file.scan.failed"

  # Consumer settings
  prefetchCount: 1

# Redis for cancellation checks
redis:
  addr: "127.0.0.1:6379"
  password: ""
  db: 0

# Temporary NFS storage
tempNFS:
  prefix: "/mnt/temp-nfs/"

# Logging configuration
logging:
  level: "info"  # trace, debug, info, warn, error, fatal, panic
```

#### Environment Variables

All configuration can be overridden with environment variables prefixed with `CLAMGO_`:

```bash
# ClamAV configuration
export CLAMGO_CLAMD_TCP_ADDR="localhost:3310"
export CLAMGO_CLAMD_UNIX_PATH=""

# RabbitMQ configuration
export CLAMGO_RABBITMQ_HOST="rabbitmq.example.com"
export CLAMGO_RABBITMQ_PORT="5672"
export CLAMGO_RABBITMQ_USERNAME="guest"
export CLAMGO_RABBITMQ_PASSWORD="guest"

# Redis configuration
export CLAMGO_REDIS_ADDR="redis.example.com:6379"
export CLAMGO_REDIS_PASSWORD="password"

# Logging
export CLAMGO_LOGGING_LEVEL="debug"

# NFS prefix
export CLAMGO_TEMPNFS_PREFIX="/mnt/temp-nfs/"
```

### Running ClamGo

#### Local Development

```bash
# Start ClamAV daemon
docker run --name clamav -d \
  -p 3310:3310 \
  -v /tmp/clamav/database:/var/lib/clamav \
  clamav/clamav:latest

# Start RabbitMQ
docker run --name rabbitmq -d \
  -p 5672:5672 \
  -p 15672:15672 \
  rabbitmq:3.13-management

# Run ClamGo
go run main.go

# Or run the built binary
./clamgo
```

#### Docker Compose

```yaml
version: '3.8'

services:
  clamav:
    image: clamav/clamav:latest
    ports:
      - "3310:3310"
    volumes:
      - clamav-db:/var/lib/clamav
      - /tmp/scandir:/scandir
    environment:
      - CLAMAV_LOG_FILE_VERBOSE=yes

  rabbitmq:
    image: rabbitmq:3.13-management
    ports:
      - "5672:5672"
      - "15672:15672"
    environment:
      - RABBITMQ_DEFAULT_USER=guest
      - RABBITMQ_DEFAULT_PASS=guest
    healthcheck:
      test: ["CMD", "rabbitmq-diagnostics", "ping"]
      interval: 30s
      timeout: 10s
      retries: 3

  redis:
    image: redis:7.2-alpine
    ports:
      - "6379:6379"
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 30s
      timeout: 10s
      retries: 3

  clamgo:
    build:
      context: .
      dockerfile: build/package/Dockerfile
    depends_on:
      - clamav
      - rabbitmq
      - redis
    environment:
      - CLAMGO_CLAMD_TCP_ADDR=clamav:3310
      - CLAMGO_RABBITMQ_HOST=rabbitmq
      - CLAMGO_RABBITMQ_PORT=5672
      - CLAMGO_REDIS_ADDR=redis:6379
      - CLAMGO_LOGGING_LEVEL=info
      - CLAMGO_TEMPNFS_PREFIX=/tmp/
    volumes:
      - /tmp:/tmp
    ports:
      - "9000:9000"  # If exposing metrics

volumes:
  clamav-db:
```

Run with:
```bash
docker-compose up -d
```

## 📊 Message Formats

### Input: File Upload Message

```json
{
  "fileId": "file-uuid-123",
  "caseId": "case-uuid-456",
  "originalName": "document.pdf",
  "contentType": "application/pdf",
  "size": 1024000,
  "tempPath": "/mnt/temp-nfs/uploads/file-uuid-123"
}
```

**RabbitMQ Queue**: `q.file.scan`  
**RabbitMQ Exchange**: `uploader.exchange`

### Output: Scan Result Message

```json
{
  "fileId": "file-uuid-123",
  "caseId": "case-uuid-456",
  "originalName": "document.pdf",
  "sha256": "abc123def456...",
  "verdict": "CLEAN",
  "signature": "",
  "scanDuration": 1200,
  "timestamp": "2025-03-01T12:00:00Z"
}
```

**RabbitMQ Routing Key**: `file.scan.completed` (success) or `file.scan.retrying` (retry)

### Cancellation Message

```json
{
  "caseId": "case-uuid-456"
}
```

**RabbitMQ Queue**: `q.case.cancelled`

## 🔧 Development

### Setting Up Development Environment

```bash
# Install development tools
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

# Run linting
golangci-lint run ./...

# Format code
go fmt ./...

# Run tests with coverage
go test -v -race -coverprofile=coverage.out ./...
go tool cover -html=coverage.out
```

### Testing ClamAV Integration

```bash
# Start ClamAV container
mkdir -p /tmp/clamav/{scandir,database,clamd}
chown -R 100:101 /tmp/clamav
chmod -R 770 /tmp/clamav

docker run --detach \
  --name=clamav \
  --mount type=bind,source=/tmp/clamav/scandir/,target=/scandir/ \
  --mount type=bind,source=/tmp/clamav/database/,target=/var/lib/clamav \
  --mount type=bind,source=/tmp/clamav/clamd/,target=/tmp/ \
  docker.io/clamav/clamav:latest

# Wait for database initialization (first run can take 5-10 minutes)
docker logs -f clamav

# Test clamd connection
nc -zv localhost 3310
```

### Code Structure

```
ClamGo/
├── main.go                 # Application entry point
├── cmd/                    # Command-line utilities (future)
├── pkg/
│   ├── scanner/           # Core scanning logic
│   │   ├── scanner.go
│   │   ├── scanner_test.go
│   │   └── ...
│   ├── service/
│   │   ├── clamd/         # ClamAV daemon client
│   │   │   ├── client.go
│   │   │   ├── scan.go
│   │   │   ├── ping.go
│   │   │   ├── version.go
│   │   │   ├── stats.go
│   │   │   └── ...
│   │   └── ...
│   ├── model/             # Data structures & protobuf definitions
│   │   ├── file_uploaded_message.go
│   │   ├── scan_result_messages.go
│   │   ├── case_cancelled_message.go
│   │   ├── clamd_*.go
│   │   └── ...
│   └── ...
├── api/
│   └── scan.proto         # Protocol buffer definitions
├── configs/
│   └── config.yaml        # Default configuration
├── build/
│   └── package/
│       └── Dockerfile     # Multi-stage Docker build
├── docs/
│   ├── arch.md           # Architecture diagrams
│   └── development.md    # Development guide
├── test/                  # Integration tests
├── .github/               # CI/CD workflows
├── go.mod & go.sum       # Dependency management
├── .golangci.yml         # Linting configuration
├── cliff.toml            # Changelog configuration
└── LICENSE               # MIT License
```

## 📈 Logging

ClamGo uses structured logging with **zerolog** for comprehensive observability. Logs include:

- **Structured fields**: fileId, caseId, retryCount, duration
- **Context information**: operation, error details, retry status
- **Performance metrics**: scan duration, message processing time
- **State changes**: case cancellations, scanner status

### Log Levels

- `trace`: Detailed debug information (most verbose)
- `debug`: Debug-level messages
- `info`: Informational messages (default)
- `warn`: Warning messages
- `error`: Error messages
- `fatal`: Fatal errors (application exits)
- `panic`: Panic-level errors

### Example Log Output

```
2025-03-01T12:00:00Z INF ClamGo started logger=root
2025-03-01T12:00:01Z INF RabbitMQ connected addr=rabbitmq:5672 logger=root
2025-03-01T12:00:02Z INF Redis connected addr=redis:6379 logger=root
2025-03-01T12:00:05Z INF starting scan consumer queue=q.file.scan logger=root
2025-03-01T12:00:10Z INF scanning file fileId=abc123 caseId=def456 logger=root
2025-03-01T12:00:11Z INF scan completed fileId=abc123 verdict=CLEAN duration=1000 logger=root
```

## 🔐 Security Considerations

### Path Validation
- All file paths are validated against a configurable prefix to prevent path traversal attacks
- Default: `/mnt/temp-nfs/` - can be configured via `tempNFS.prefix`

### Access Control
- RabbitMQ credentials should be managed via environment variables
- Redis credentials should be secured in production
- Use strong credentials for message queue access

### File Handling
- Files are opened with read-only permissions
- Temporary files are not modified by ClamGo
- File cleanup is delegated to the caller

### Container Security
- ClamGo runs as non-root user (uid: 1000) in Docker
- No secrets are hardcoded
- Health checks monitor service availability

## 🐛 Troubleshooting

### ClamAV Connection Issues

```bash
# Check clamd is running
docker ps | grep clamav

# Test clamd connectivity
nc -zv localhost 3310

# View clamd logs
docker logs clamav

# Verify configuration
echo "PINGn" | nc localhost 3310
```

### RabbitMQ Connection Issues

```bash
# Check RabbitMQ is running
docker ps | grep rabbitmq

# Check RabbitMQ management UI
# http://localhost:15672

# Verify credentials
# Default: guest / guest

# Monitor queue depth
rabbitmqctl list_queues
```

### Common Errors

**"failed to connect to RabbitMQ"**
- Verify RabbitMQ is running and accessible
- Check credentials in configuration
- Verify network connectivity

**"clamd unavailable"**
- Check clamd container is running
- Verify ClamAV database is initialized
- Check port mapping (default: 3310)

**"temp path does not match required prefix"**
- Verify file path starts with configured prefix
- Check `tempNFS.prefix` setting
- Ensure shared storage is mounted correctly

## 📦 Dependencies

Key dependencies include:

- **BunnyShepherd**: RabbitMQ connection management library
- **rabbitmq/amqp091-go**: AMQP protocol implementation
- **redis/go-redis**: Redis client
- **zerolog**: Structured logging
- **viper**: Configuration management
- **gabriel-vasile/mimetype**: File type detection
- **testcontainers-go**: Integration testing with containers

See `go.mod` for complete dependency list.

## 📝 Contributing

Contributions are welcome! Please follow these guidelines:

1. **Commit Format**: Use conventional commits
   - `feat:` for new features
   - `fix:` for bug fixes
   - `docs:` for documentation
   - `test:` for tests
   - `refactor:` for code refactoring

2. **Code Quality**: 
   - Run `golangci-lint run ./...` before submitting
   - Ensure tests pass: `go test -v ./...`
   - Maintain >80% code coverage

3. **Pull Requests**:
   - Create descriptive PR titles
   - Reference related issues
   - Include test cases for new functionality

## 📚 Documentation

- **[Architecture Guide](./docs/arch.md)** - System design and flow diagrams
- **[Development Guide](./docs/development.md)** - Setup and development instructions
- **[CI/CD Documentation](./.github/CI_CD_GUIDE.md)** - Pipeline and deployment
- **[Configuration Reference](#configuration)** - Configuration options above

## 📊 Monitoring & Observability

### Metrics Exposed

ClamGo provides metrics for monitoring:

- Scan success/failure rates
- Average scan duration
- Queue depth (via RabbitMQ)
- clamd connection status
- Redis connectivity status

### Integration Points

- **Prometheus** (optional): Export metrics for monitoring
- **Jaeger** (optional): Distributed tracing support
- **ELK Stack**: Centralized logging integration

## 🔄 Retry Strategy

ClamGo implements a sophisticated retry mechanism:

```
Initial Attempt
    ↓
    ├─ Success → Publish result (file.scan.completed)
    ├─ Transient Error → NACK & Retry
    │
    └─ Retry 1 (30 seconds)
        ├─ Success → Publish result
        ├─ Error → Route to Retry 2
        │
        └─ Retry 2 (120 seconds)
            ├─ Success → Publish result
            ├─ Error → Route to Retry 3
            │
            └─ Retry 3 (600 seconds)
                ├─ Success → Publish result
                └─ Error → Route to DLQ (file.scan.failed)
```

## 🚢 Deployment

### Kubernetes

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: clamgo
spec:
  replicas: 3
  selector:
    matchLabels:
      app: clamgo
  template:
    metadata:
      labels:
        app: clamgo
    spec:
      containers:
      - name: clamgo
        image: ghcr.io/clamgo/clamgo:v1.0.0
        env:
        - name: CLAMGO_CLAMD_TCP_ADDR
          value: "clamd.antivirus.svc:3310"
        - name: CLAMGO_RABBITMQ_HOST
          value: "rabbitmq.messaging.svc"
        - name: CLAMGO_REDIS_ADDR
          value: "redis.cache.svc:6379"
        resources:
          requests:
            cpu: 500m
            memory: 512Mi
          limits:
            cpu: 2000m
            memory: 2Gi
        livenessProbe:
          tcpSocket:
            port: 3310
          initialDelaySeconds: 30
          periodSeconds: 10
        readinessProbe:
          tcpSocket:
            port: 3310
          initialDelaySeconds: 5
          periodSeconds: 5
```

## 📄 License

This project is licensed under the MIT License - see the [LICENSE](./LICENSE) file for details.

## 🙋 Support

For issues, questions, or contributions:

- **GitHub Issues**: Report bugs and feature requests
- **Discussions**: Ask questions and share ideas
- **Security**: Report security issues responsibly (see SECURITY.md)

## 🎉 Acknowledgments

ClamGo is built on excellent open-source projects:

- **ClamAV**: Open-source virus scanning engine
- **RabbitMQ**: Robust message broker
- **Go**: Fast, efficient programming language
- **BunnyShepherd**: RabbitMQ connection management

---

**Made with ❤️ by the ClamGo team**

For more information, visit [ClamGo Documentation](https://github.com/ClamGo/clamgo/wiki)
