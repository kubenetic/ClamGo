# ClamGo CI/CD Pipeline Documentation

This document describes the comprehensive CI/CD pipeline set up for the ClamGo project following DevOps best practices.

## 🏗️ Pipeline Architecture

The CI/CD pipeline consists of multiple automated workflows designed to ensure code quality, security, and reliable deployments.

### Workflows Overview

| Workflow | Trigger | Purpose |
|----------|---------|---------|
| **Build and Release** | Push to main/develop/tags | Build, test, and create releases |
| **Docker Publish** | Push to main/develop/tags | Build and publish Docker images |
| **Code Quality** | Push/PR to main/develop | Lint and format checks |
| **Security Scanning** | Push/PR to main/develop, daily | Dependency and code security analysis |
| **Pull Request Checks** | Pull requests | Validate commits and PR size |

## 🔄 Build and Release Workflow

**File:** `.github/workflows/build-and-release.yml`

### Features

- ✅ Multi-version Go testing (1.25)
- ✅ Comprehensive linting with golangci-lint
- ✅ Test coverage reporting to Codecov
- ✅ Multi-platform binary compilation:
  - Linux (AMD64, ARM64)
  - macOS (AMD64, ARM64)
  - Windows (AMD64)
- ✅ Automated changelog generation
- ✅ GitHub Release creation with binaries
- ✅ Build artifact retention (30 days)

### Binary Naming Convention

- `clamgo-linux-amd64` - Linux x86_64
- `clamgo-linux-arm64` - Linux ARM64
- `clamgo-darwin-amd64` - macOS Intel
- `clamgo-darwin-arm64` - macOS Apple Silicon
- `clamgo-windows-amd64.exe` - Windows x86_64

### Version Information

Binaries are compiled with version and build time information:

```go
-ldflags="-X main.Version=<ref_name> -X main.BuildTime=<timestamp>"
```

Ensure your `main.go` has the following variables:

```go
var (
    Version   = "dev"
    BuildTime = "unknown"
)
```

### Release Process

1. **Tag Creation**: Create a semantic version tag (e.g., `v1.0.0`)
2. **Workflow Triggers**: Build and release workflow automatically starts
3. **Tests & Build**: Code is tested, linted, and compiled
4. **Release Creation**: GitHub release is created with:
   - All compiled binaries
   - Generated changelog
   - Pre-release flag for alpha/beta/rc versions

## 🐳 Docker Publish Workflow

**File:** `.github/workflows/docker-publish.yml`

### Features

- ✅ Multi-architecture builds (AMD64, ARM64)
- ✅ Build dependency on successful app build
- ✅ GitHub Container Registry (GHCR) integration
- ✅ Automatic image tagging:
  - Branch names (e.g., `main`, `develop`)
  - Semantic versions (e.g., `1.0.0`, `1.0`, `1`)
  - Short commit SHA with branch prefix
- ✅ Layer caching for faster builds
- ✅ Image inspection and verification

### Image Registry

Images are pushed to: `ghcr.io/<owner>/clamgo`

### Image Tags

For a tag `v1.0.0`:
- `ghcr.io/owner/clamgo:1.0.0` (Full version)
- `ghcr.io/owner/clamgo:1.0` (Minor version)
- `ghcr.io/owner/clamgo:1` (Major version)
- `ghcr.io/owner/clamgo:sha-<commit_sha>` (Commit reference)

### Pulling Images

```bash
# Latest main branch
docker pull ghcr.io/<owner>/clamgo:main

# Latest develop branch
docker pull ghcr.io/<owner>/clamgo:develop

# Specific version
docker pull ghcr.io/<owner>/clamgo:1.0.0

# By commit
docker pull ghcr.io/<owner>/clamgo:main-abc1234
```

## 🔍 Code Quality Checks

**File:** `.github/workflows/lint.yml`

### Checks Performed

- **gofmt**: Code formatting validation
- **go vet**: Go semantics analysis
- **staticcheck**: Static analysis for bugs and inefficiencies
- **gosec**: Security issue detection

### Configuration

Linting is configured via `.golangci.yml` with comprehensive rule sets covering:
- Code style and conventions
- Performance and efficiency
- Security concerns
- Error handling
- Documentation

## 🛡️ Security Scanning

**File:** `.github/workflows/security.yml`

### Scans Included

1. **Dependency Vulnerabilities**
   - Daily scheduled scan
   - Uses `govulncheck`
   - Checks for known CVEs in dependencies

2. **Go Mod Validation**
   - Ensures dependencies are up to date
   - Validates `go.mod` and `go.sum` integrity

3. **CodeQL Analysis**
   - GitHub's advanced code analysis
   - Detects security and quality issues
   - Results stored in Security tab

## 📋 Pull Request Checks

**File:** `.github/workflows/pull-request.yml`

### Validations

- ✅ Conventional commit format
- ✅ Merge conflict detection
- ✅ Test execution
- ✅ Large file detection (>5MB warning)
- ✅ Documentation update suggestions
- ✅ PR size analysis (warning for >1000 lines)

### Conventional Commit Format

```
<type>(<scope>): <subject>

<body>

<footer>
```

**Types:**
- `feat` - New feature
- `fix` - Bug fix
- `docs` - Documentation
- `style` - Code style (formatting, missing semicolons, etc.)
- `refactor` - Code refactoring
- `perf` - Performance improvement
- `test` - Adding or updating tests
- `chore` - Build process, dependency updates, tooling

**Examples:**
```
feat(api): add user authentication endpoint
fix(scanner): resolve race condition in file processing
docs: update installation instructions
chore(deps): update go dependencies
```

## 📦 Dockerfile

**File:** `build/package/Dockerfile`

### Features

- ✅ Multi-stage build for minimal image size
- ✅ Alpine Linux for small footprint
- ✅ Non-root user execution (UID 1000)
- ✅ Health checks
- ✅ Proper signal handling with dumb-init
- ✅ OpenContainer image labels

### Image Size Optimization

- Stage 1: Go builder - compiles application
- Stage 2: Alpine runtime - only includes necessary components

### Health Check

```dockerfile
HEALTHCHECK --interval=30s --timeout=10s --start-period=40s --retries=3 \
    CMD nc -z localhost 3310 || exit 1
```

### Usage

```bash
# Build image locally
docker build -f build/package/Dockerfile -t clamgo:local .

# Run container
docker run -d \
  --name clamgo \
  -p 3310:3310 \
  -e CLAMGO_CLAMD_TCP_ADDR="clamd:3310" \
  ghcr.io/owner/clamgo:latest
```

## 🔐 Permissions and Secrets

### Required Permissions

The workflows use GitHub token automatically provided:
- `contents: write` - For releases and commits
- `packages: write` - For Docker image push
- `security-events: write` - For CodeQL reporting

### No Manual Secrets Required

All workflows use the built-in `secrets.GITHUB_TOKEN` which is automatically available and scoped appropriately.

## 📚 Configuration Files

### `.golangci.yml`
Comprehensive linter configuration with 50+ enabled linters for maximum code quality.

### `cliff.toml`
Changelog generation configuration based on Conventional Commits.

### `.github/dependabot.yml`
Automated dependency update configuration for:
- Go modules (weekly)
- GitHub Actions (weekly)
- Docker images (weekly)

## 🚀 Deployment Flow

```
Git Push/Tag
    ↓
Build & Test Workflow
    ├─ Lint Code
    ├─ Run Tests
    ├─ Compile Binaries
    └─ Create Release (if tagged)
    ↓
Docker Workflow (waits for build success)
    ├─ Build Image
    ├─ Multi-arch support
    └─ Push to GHCR
    ↓
Image Available for Deployment
```

## 🔧 Local Development

### Pre-commit Setup

Install git hooks to catch issues early:

```bash
# Install golangci-lint
go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest

# Run linter locally
golangci-lint run ./...

# Format code
go fmt ./...

# Run tests
go test -v ./...
```

### Testing Locally

```bash
# Run all tests
go test -v -race -coverprofile=coverage.out ./...

# View coverage
go tool cover -html=coverage.out

# Test specific package
go test -v ./pkg/scanner
```

## 📊 Monitoring and Notifications

### GitHub Actions Status

- **Build Status**: Visible in repo README with badge
- **PR Checks**: Block merge if workflows fail
- **Release Notes**: Auto-generated from commits

### Codecov Integration

Coverage reports are uploaded and available at codecov.io

## 🐛 Troubleshooting

### Build Failures

1. Check workflow logs in Actions tab
2. Verify Go version compatibility
3. Check for lint errors with `golangci-lint run`

### Docker Push Failures

- Ensure `secrets.GITHUB_TOKEN` has `packages: write` permission
- Verify Docker buildx is properly configured
- Check image naming follows GHCR conventions

### PR Check Failures

- Validate conventional commit format
- Run tests locally: `go test -v ./...`
- Format code: `go fmt ./...`

## 📝 Best Practices

1. **Semantic Versioning**: Follow SemVer for version tags (v1.2.3)
2. **Conventional Commits**: Use standardized commit messages
3. **Code Coverage**: Aim for >80% test coverage
4. **Pull Requests**: Keep PRs focused and <1000 lines when possible
5. **Documentation**: Update docs when changing functionality
6. **Security**: Address dependabot alerts promptly

## 🔗 References

- [Conventional Commits](https://www.conventionalcommits.org/)
- [Semantic Versioning](https://semver.org/)
- [Keep a Changelog](https://keepachangelog.com/)
- [GitHub Actions Documentation](https://docs.github.com/en/actions)
- [golangci-lint](https://golangci-lint.run/)
- [git-cliff](https://git-cliff.org/)

## 📞 Support

For issues or questions about the CI/CD pipeline:

1. Check workflow logs in GitHub Actions
2. Review recent commits for changes
3. Open an issue with pipeline logs
