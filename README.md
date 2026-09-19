# {{REPO_NAME}}

Go monorepo template

## Architecture

```
{{REPO_NAME}}/
├── apps/example/              # Main example application
├── pkg/                       # Shared utilities & vendors
│   ├── atexit/                # At-exit handler
│   ├── hertz/client/          # Hertz HTTP client helpers
│   ├── hertz/doer/            # Hertz request doer abstraction
│   ├── logs/                  # Structured logging
│   ├── shutdown/              # Graceful shutdown orchestration
│   └── versionhelper/         # Build version embedding
└── go.work                    # Go workspace (all modules linked)
```

Each directory with a `go.mod` is an independent module under `github.com/{{GITHUB_REPOSITORY}}/...`. The root `go.work` ties them together for local development.

## Prerequisites

- Go 1.26+
- Make

## Development Setup

```bash
# Clone and sync all modules
git clone git@github.com:{{GITHUB_REPOSITORY}}.git
cd {{REPO_NAME}}

mv -f .github/_workflows/*.*  .github/workflows/
rm -rf .github/_workflows

make go-mod-tidy

# Sync tags
git fetch --prune --prune-tags origin

# Setup go workspace
go work init
go work use -r .

# Install tools + generate code + run linters + checks
make development-checks
```

## List all make command

```bash
make help
```


## Running Tests

```bash
make test
```

## Linting & Static Analysis

```bash
make go-lint              # golangci-lint across all modules
make go-staticcheck       # staticcheck across all modules
make go-govulncheck       # vulnerability check across all modules
```

## Code Generation

```bash
make go-mockery           # Generate mocks
make wire <module>        # Dependency injection wiring
make hz <args>            # Hertz code generation
make buf <args>           # Protobuf/gRPC generation
```


## Releases Version

```bash
# Tag and push a module release to trigger deployment
make release MODULE=pkg/atexit VERSION=v0.0.1

# List modules and their latest tags
make release-list
```
