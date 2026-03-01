---
# ClamGo CI/CD Architecture
---

## Pipeline Architecture Overview

```
┌─────────────────────────────────────────────────────────────────────────┐
│                          Git Repository Events                          │
│                  (Push to main/develop, Tags, PRs)                      │
└─────────────────────────────────────────────────────────────────────────┘
                                    │
                    ┌───────────────┼───────────────┐
                    │               │               │
                    ▼               ▼               ▼
            ┌──────────────┐  ┌──────────────┐  ┌──────────────┐
            │   PR Check   │  │   Security   │  │  Code Lint   │
            │  Workflow    │  │   Scanning   │  │  Workflow    │
            └──────────────┘  └──────────────┘  └──────────────┘
                    │               │               │
                    └───────────────┼───────────────┘
                                    │
                    ┌───────────────▼───────────────┐
                    │  Build & Release Workflow     │
                    │  • Lint (golangci-lint)       │
                    │  • Unit Tests (race detector) │
                    │  • Multi-platform Build       │
                    │  • Coverage Report (Codecov)  │
                    │  • Create Release (if tagged) │
                    └───────────────┬───────────────┘
                                    │
                    ┌───────────────▼───────────────┐
                    │   BUILD SUCCESS?              │
                    │  ✅ YES → Continue           │
                    │  ❌ NO → Stop (Fail)         │
                    └───────────────┬───────────────┘
                                    │
                    ┌───────────────▼───────────────┐
                    │  Docker Build Workflow        │
                    │  • Multi-arch build           │
                    │  • Layer caching              │
                    │  • Push to GHCR               │
                    │  • Image verification         │
                    └───────────────┬───────────────┘
                                    │
                    ┌───────────────▼───────────────┐
                    │  Docker Image Published       │
                    │  ghcr.io/owner/clamgo:tags   │
                    └───────────────────────────────┘
```

## Workflow Triggers

```
Event Type          │ Build  │ Docker │ Lint   │ Security │ PR Check
────────────────────┼────────┼────────┼────────┼──────────┼──────────
Push main           │   ✅   │   ✅   │   ✅   │    ✅    │    -
Push develop        │   ✅   │   ✅   │   ✅   │    ✅    │    -
Push tag (v*)       │   ✅   │   ✅   │   ✅   │    ✅    │    -
Pull request        │   ✅   │   -    │   ✅   │    ✅    │    ✅
Scheduled (daily)   │   -    │   -    │   -    │    ✅    │    -
Dependabot update   │   ✅   │   ✅   │   ✅   │    ✅    │    ✅
```

## Parallel Execution Strategy

```
Push to main/develop/tag
│
├─────────────────────────────────────────────────────────┐
│                                                         │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  │
│  │  PR Checks   │  │   Security   │  │   Linting    │  │
│  │  (0-5 min)   │  │  (3-8 min)   │  │  (2-4 min)   │  │
│  └──────────────┘  └──────────────┘  └──────────────┘  │
│                                                         │
│  All run in parallel                                   │
│
└─────────────────────────────────────────────────────────┘
│
├─────────────────────────────────────────────────────────┐
│  Main Build Workflow (wait for above to finish)        │
│  • Lint Code         (2-3 min)                         │
│  • Run Tests         (3-5 min)                         │
│  • Build Binaries    (4-6 min)                         │
│  Total: ~10-15 min                                     │
└─────────────────────────────────────────────────────────┘
│
├── SUCCESS ──→ Docker Build Workflow (5-10 min)
│              • Build multi-arch
│              • Push to GHCR
│              • Verify image
│
└── FAILURE ──→ Stop (Notify)
```

## Data Flow

```
Source Code
    │
    ├─→ Linter/Formatter Check
    │   └─→ ✅ Pass/❌ Fail
    │
    ├─→ Unit Tests + Coverage
    │   └─→ ✅ Pass/❌ Fail
    │
    ├─→ Cross-platform Compilation
    │   ├─→ Linux AMD64 Binary
    │   ├─→ Linux ARM64 Binary
    │   ├─→ macOS AMD64 Binary
    │   ├─→ macOS ARM64 Binary
    │   └─→ Windows AMD64 Binary
    │
    ├─→ Release Creation (if tagged)
    │   └─→ GitHub Release with Binaries + Changelog
    │
    └─→ Docker Image Build (if build passed)
        ├─→ Multi-stage build
        ├─→ Multi-arch image (AMD64, ARM64)
        ├─→ Layer caching
        └─→ Push to ghcr.io
```

## Build Artifacts

```
Build Outputs
├── Binaries
│   ├── clamgo-linux-amd64      (Linux x86_64)
│   ├── clamgo-linux-arm64      (Linux ARM64)
│   ├── clamgo-darwin-amd64     (macOS Intel)
│   ├── clamgo-darwin-arm64     (macOS Apple Silicon)
│   └── clamgo-windows-amd64.exe (Windows x86_64)
│
├── Coverage Report
│   └── coverage.out + Codecov Upload
│
├── GitHub Release (on tagged release)
│   ├── Release Notes (auto-generated)
│   ├── All binaries attached
│   └── Changelog included
│
└── Docker Images
    ├── ghcr.io/owner/clamgo:main
    ├── ghcr.io/owner/clamgo:develop
    ├── ghcr.io/owner/clamgo:v1.0.0
    ├── ghcr.io/owner/clamgo:1.0
    ├── ghcr.io/owner/clamgo:1
    └── ghcr.io/owner/clamgo:main-<commit_sha>
```

## Security Scanning Layers

```
┌─────────────────────────────────────────┐
│      Security Scanning Pipeline         │
├─────────────────────────────────────────┤
│ Layer 1: Dependency Scanning            │
│  └─→ govulncheck (CVE detection)       │
│  └─→ go mod tidy (dependency integrity)│
├─────────────────────────────────────────┤
│ Layer 2: Code Analysis                  │
│  └─→ gosec (secret detection)           │
│  └─→ CodeQL (vulnerability patterns)    │
├─────────────────────────────────────────┤
│ Layer 3: Linting Rules                  │
│  └─→ 50+ golangci-lint rules            │
│  └─→ Security-focused rule set          │
├─────────────────────────────────────────┤
│ Layer 4: Automated Updates              │
│  └─→ Dependabot (dependency updates)    │
└─────────────────────────────────────────┘
```

## Performance Characteristics

### Build Times
- **Lint Check**: 2-3 minutes
- **Unit Tests**: 3-5 minutes (with caching)
- **Build Binaries**: 4-6 minutes
- **Docker Build**: 5-10 minutes (with layer caching)
- **Total Pipeline**: ~15-25 minutes (running in parallel)

### Storage
- **Build Artifacts**: 30 days retention
- **GitHub Release**: Permanent
- **Docker Images**: Per tag/branch retention policy
- **Coverage Reports**: Uploaded to Codecov

### Resource Usage
- **Runners**: ubuntu-latest (standard GitHub-hosted runners)
- **Parallel Jobs**: Up to 20 concurrent workflows
- **Disk Space**: ~5GB per workflow run
- **Execution**: Typically <30 minutes total

## Deployment Checklist

Before deploying to production:

```
□ All CI/CD workflows pass
□ Tests pass with >80% coverage
□ Code review approved
□ Security scan cleared
□ Changelog updated
□ Version tagged properly
□ Documentation updated
□ Dependencies up to date
□ Docker image built and tested
□ Release notes prepared
```

## Monitoring and Alerts

```
Critical Failures
├─→ Build failures → Slack/email notification
├─→ Security findings → GitHub Security tab
├─→ Dependency vulnerabilities → Dependabot PR
└─→ Docker push failures → Workflow run logs

Non-Critical
├─→ Large PR warnings
├─→ Documentation suggestions
└─→ Linting warnings (non-blocking)
```

## Version Tagging Strategy

```
Semantic Versioning: v<MAJOR>.<MINOR>.<PATCH>

Examples:
v1.0.0          → Full Release
v1.0.0-alpha    → Alpha Release
v1.0.0-beta     → Beta Release
v1.0.0-rc.1     → Release Candidate
v1.0.0-dev      → Development Build

Pre-release versions are marked as such in GitHub Releases
```

## Branch Strategy

```
main              → Production releases (stable)
develop           → Development branch (pre-release)
feature/*         → Feature branches (PR → develop)
bugfix/*          → Bug fix branches (PR → develop)
release/*         → Release branches (from develop → main)
hotfix/*          → Critical fixes (from main → main/develop)
```

All workflows run on feature branches too for early validation!
