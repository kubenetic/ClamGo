# ClamGo Workflow Status

Below are the status badges for the ClamGo project's CI/CD pipelines:

## Build Status

[![Build and Release](https://github.com/ClamGo/clamgo/actions/workflows/build-and-release.yml/badge.svg?branch=main)](https://github.com/ClamGo/clamgo/actions/workflows/build-and-release.yml)
[![Docker Publish](https://github.com/ClamGo/clamgo/actions/workflows/docker-publish.yml/badge.svg?branch=main)](https://github.com/ClamGo/clamgo/actions/workflows/docker-publish.yml)
[![Code Quality](https://github.com/ClamGo/clamgo/actions/workflows/lint.yml/badge.svg?branch=main)](https://github.com/ClamGo/clamgo/actions/workflows/lint.yml)
[![Security Scanning](https://github.com/ClamGo/clamgo/actions/workflows/security.yml/badge.svg?branch=main)](https://github.com/ClamGo/clamgo/actions/workflows/security.yml)

## Quick Reference

### Creating a Release

1. Tag the commit with semantic version:
   ```bash
   git tag -a v1.0.0 -m "Release version 1.0.0"
   git push origin v1.0.0
   ```

2. The build-and-release workflow will:
   - Run linting and tests
   - Compile binaries for all platforms
   - Generate changelog from commits
   - Create GitHub Release with binaries

### Docker Images

Pull images from GitHub Container Registry:

```bash
# Latest main
docker pull ghcr.io/clamgo/clamgo:main

# Specific version
docker pull ghcr.io/clamgo/clamgo:1.0.0

# By commit SHA
docker pull ghcr.io/clamgo/clamgo:main-abc1234def5678
```

### Running Locally

Build and run locally:

```bash
# Build
go build -o clamgo .

# Run with local config
./clamgo

# Run with environment variables
CLAMGO_CLAMD_TCP_ADDR=localhost:3310 ./clamgo
```

### Commit Message Format

All commits must follow Conventional Commits:

```
<type>(<scope>): <description>

<body>

<footer>
```

**Valid types:** feat, fix, docs, style, refactor, perf, test, chore

Example:
```
feat(scanner): add parallel file scanning support

This commit implements concurrent file scanning to improve throughput.
Large files are processed in parallel batches.

Closes #123
```

### Dependency Updates

Dependabot automatically creates PRs for:
- Go module updates (weekly)
- GitHub Actions updates (weekly)
- Docker base image updates (weekly)

Review and merge these PRs to keep dependencies current.
