# Contributing to wrapi

Thanks for helping turn any REST API into an agent-native one. 🚦

## Project layout

- `compiler/` — the design-time Node build (`compiler.js`) + the `wrapi` CLI (`bin/wrapi.js`).
- `gateway/` — the run-time Go reverse proxy (`main.go`, `composite.go`).
- `package.json` (root) — the npm manifest (`bin: wrapi`) and the Node test script.

## Setup

```bash
make deps        # npm install (root) + go mod tidy (gateway)
```

Prereqs: **Node ≥ 18**, **Go ≥ 1.22**.

## Run the tests

```bash
make test        # node --test  +  go test ./...
```

Please keep both suites green and run `gofmt -w gateway/` before committing (CI
enforces `gofmt`).

## Guidelines

- **Statelessness is the core promise.** New runtime features must not introduce
  shared cross-request state. Enforce via signed tokens or deterministic
  transforms (see the deferred `rate_limit`/`budget_cap`/`idempotency` note in
  the [ROADMAP](./ROADMAP.md)).
- **Design-time LLM, run-time determinism.** The gateway never calls a model on
  the hot path.
- Add tests for new behavior (Go: `gateway/*_test.go`, Node: `compiler/*.test.js`).
- Match the surrounding code style; keep changes focused.

## Good first contributions

- Additional condition operators or policy types (stateless ones).
- Compiler auto-proposal of composites / response shapes.
- Example configs for popular public APIs.
- The eval harness for tool-selection accuracy (Phase 5).

See the [ROADMAP](./ROADMAP.md) for where things are headed.
