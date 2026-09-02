# AGENTS.md

Instructions for coding agents working in this repository.

## What this is

godwit is a crash-safe PostgreSQL migration service written in Go: a statement-level journal in the target database, a leased scheduler that survives replica death, and a connect (gRPC + JSON) API. Read `README.md` for the feature map before touching anything.

## Layout

```
api/proto/godwit/v1/   protobuf API (buf); regenerate with `buf generate`, never edit gen/
internal/engine/       loader, planner (libpg_query), executor, journal, verifiers
internal/controlplane/ store, scheduler, rollout policies, drift monitor, validator
internal/api/          connect handlers and auth
internal/server/       wiring; end-to-end tests live here
internal/creds/        credential providers + credstest conformance suite
demo/                  docker compose demo (two replicas, kill -9 recovery)
```

## Commands

```
make all          lint, proto-lint, coverage gate, build — run before every commit
make cover        ./scripts/coverage.sh: 100% of statements, no exceptions
make lint         golangci-lint (revive exported, gofumpt, goimports)
buf generate      after any .proto change (needs $(go env GOPATH)/bin on PATH)
```

Tests use testcontainers (postgres:17-alpine); Docker must be running.

## Hard rules

- **Comments: none by default.** Write one only when the reader cannot get it from the next line — a lint-required doc on an exported identifier, or a non-obvious constraint (ordering, race, side effect). One line, always. Never narrate test steps, never document private helpers that just repeat their name, never reference plans, phases or PRs in code. Grep `^\s*//` in the files you touched before committing and cut.
- **100% coverage** is a gate, not a target. Design for it: thin interfaces at every I/O edge, injection points for unreachable error branches.
- One feature per PR; merge before starting the next. Never change `.github/workflows/ci.yml`.
- English everywhere: code, comments, commits, PRs.
- `DESIGN.md` and `PLAN.md` are local and gitignored — never commit them or mention them in code.
- A new capability lands with its store migration (`internal/controlplane/schema.go`), proto change, handler, tests at store/scheduler/API level, README row and demo step.
