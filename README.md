> [!WARNING]
> **Work in progress:** This project is under active development and is subject to change.

<div align="center">

# 🚦 wrapi

### Turn any REST API into one your AI agents can actually use.

**`wrapi` wraps your existing API in a gateway built for AI agents — easier to call, far cheaper in tokens, and safe by default. An LLM is used once, at build time, to generate the config; the running gateway never calls an LLM.**

<br/>

[![CI](https://github.com/dspachos/wrapi/actions/workflows/ci.yml/badge.svg)](https://github.com/dspachos/wrapi/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go&logoColor=white)](https://go.dev)
[![Node](https://img.shields.io/badge/Node-18+-339933?logo=node.js&logoColor=white)](https://nodejs.org)
[![OpenAI-compatible](https://img.shields.io/badge/LLM-build--time%20only-412991?logo=openai&logoColor=white)](#-how-it-works)
[![Stateless](https://img.shields.io/badge/state-zero%20💾-ff5c8a)](#-why-stateless-the-jwt-is-the-state)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](#-license)
[![PRs Welcome](https://img.shields.io/badge/PRs-welcome-brightgreen.svg)](#-contributing)

</div>

---

> **TL;DR** — Most REST APIs were built for human developers, not AI agents: cryptic names, huge specs, chatty multi-call workflows, and dangerous write endpoints. `wrapi` fixes that *without touching your API*. A one-time build step points an LLM at your `openapi.json` and compiles a compact config. A small Go gateway then sits in front of your API and gives agents a **cleaner, cheaper, safer** surface — clean tool names, trimmed responses, and human approval on risky calls — with **no LLM at request time** and **no database**.

## 🧨 The problem — APIs weren't built for agents

Your REST API was designed for human developers reading docs. Point an AI agent at it and three things go wrong:

- 🧠 **Hard to use** — mangled operation names, granular endpoints, and giant specs make the agent pick the wrong call, or fail to chain calls together.
- 💸 **Expensive** — the agent pays tokens for the whole spec *and* for every bloated response it reads back, on every step of its loop.
- 🛡️ **Dangerous** — one hallucinated call can delete a resource, move money, or grant `is_admin: true`. There is no undo.

## ✅ The solution — an agent-native gateway

`wrapi` puts a thin gateway in front of your existing API that makes it **agent-native** — without changing a line of your backend:

- 🧠 **Easier** — clean, semantic tool names + descriptions, and optional composite endpoints that collapse multi-call workflows into one.
- 💸 **Cheaper** — a stripped spec for the agent to load, and trimmed responses on the way back. Real, measured token savings.
- 🛡️ **Safer** — a typed policy engine: human approval on risky calls, plus redaction, dry-run, and audit.

> **The LLM is used exactly once — at build time — to generate the config.**
> The running gateway is pure Go: fast, predictable, and it **never calls an LLM**. No database, no cache, no shared state.

## 🗺️ How it works

Two phases. The first uses an LLM and runs **once**. The second runs on **every request** and uses **no LLM**.

```text
DESIGN TIME  —  runs ONCE  ·  the only step that uses an LLM
──────────────────────────────────────────────────────────────────

   your openapi.json  ──▶  wrapi compile  ──▶  small config files
   (big, human-first)      (an LLM reads         · agent_openapi.json    clean, small spec
                            your whole API,       · policy_map.json       guardrails
                            one time)             · response_shapes.json  response trims

                                  │  loaded once, at boot
                                  ▼

RUN TIME  —  every request  ·  NO LLM  ·  no database
──────────────────────────────────────────────────────────────────

   AI agent  ──▶  wrapi gateway (Go)  ──▶  your existing API
             ◀──    (pure, fast Go)   ◀──

   on the way through, the gateway:
     · serves a clean, small spec at GET /openapi.json
     · gives endpoints agent-friendly names + descriptions
     · asks a human to approve risky calls
     · trims & redacts responses          → fewer tokens
     · turns messy errors into { error, message, fix }

   the agent only ever talks to the gateway — never the origin directly
```

The agent points at the gateway, discovers the clean spec at `GET /openapi.json`, and makes normal HTTP calls. The gateway applies your policies, forwards allowed calls to the origin, and trims what comes back.

## ✨ What makes it different

**Easier for agents**

| | |
|---|---|
| 🏷️ **Clean tool names** | Rewrites mangled `operationId`s (`internal_provision_key_..._post`) into semantic names (`provision_private_ai_key`) with agent-friendly descriptions. |
| 🧬 **Composite operations** | Collapse a multi-call workflow (create team → set budget) into one agent-callable endpoint, with templating and rollback-on-failure. |
| 📜 **Serves its own spec** | The gateway hosts the stripped `openapi.json`, so agents discover exactly the surface it fronts — and nothing else. |

**Cheaper in tokens**

| | |
|---|---|
| 💸 **Token-frugal** | Strips the spec the agent loads **and** trims every response it reads back. The compiler reports the context reduction; the gateway logs per-call savings. |
| 🩹 **Agent-friendly errors** | Turns every upstream 4xx/5xx into one small `{error, message, fix}` envelope — actionable, and *without* leaking 5xx stack traces (fewer wasted retries). |

**Safe by default**

| | |
|---|---|
| 🛡️ **Typed policy engine** | Human approval (HITL) is one type; also `audit`, `dry_run`, and `pii_redact` — composable per endpoint. |
| 🔐 **Tamper-proof approval** | Approval is a signed JWT bound to the exact request (SHA-256 of the body). Change the payload after approval and the gateway refuses. |

**How it's built**

| | |
|---|---|
| 🤖 **LLM only at build time** | An LLM (any OpenAI-compatible endpoint) compiles the config **once**. The running gateway is pure Go and never calls an LLM. |
| 💾 **Truly stateless** | No database, no cache, no pending-request store. Any replica handles any request — scale freely. |
| ⚡ **Fast & small** | `go-chi` + `net/http`. A single static binary / distroless container. Handles huge specs by batching + `$ref` pruning at compile time. |

## 🚀 Quickstart

<details open>
<summary><b>1 · Clone & install</b></summary>

```bash
git clone https://github.com/dspachos/wrapi.git
cd wrapi
make deps        # npm install (compiler) + go mod tidy (gateway)
```

Prereqs: **Node ≥ 18** and **Go ≥ 1.22**.

</details>

<details open>
<summary><b>2 · Compile a policy from a live spec</b></summary>

```bash
# LLM connection comes from the environment:
export LLM_BASE_URL="https://api.openai.com/v1"
export LLM_API_KEY="sk-..."
export LLM_MODEL="gpt-4o"          # optional

# The spec URL is a CLI argument; the output dir is an optional second argument
# (defaults to gateway/config).
node compiler/bin/wrapi.js https://api.example.com/openapi.json
#   → gateway/config/hitl_policy_map.json   (the interception rules)
#   → gateway/config/agent_openapi.json     (the stripped agent spec)

# Write elsewhere, or drive it via make:
node compiler/bin/wrapi.js https://api.example.com/openapi.json ./out
make compile URL=https://api.example.com/openapi.json
```

> **Prefer a `wrapi` command?** Run `npm link` once (in the repo root) to put
> `wrapi` on your `PATH`, then just `wrapi <url>`. Or run it straight from GitHub
> without cloning: `npx github:dspachos/wrapi <url> ./out`.

</details>

<details open>
<summary><b>3 · Run the gateway</b></summary>

```bash
export JWT_SECRET="a-long-random-secret-key"
export LEGACY_API_URL="https://api.example.com"   # upstream to proxy to

make run
# 🚦 hitl-gateway listening on :8080
```

Or with Docker (the image bakes in `gateway/config/`):

```bash
make docker    # builds the `wrapi` image
docker run -p 8080:8080 -e JWT_SECRET=... -e LEGACY_API_URL=https://api.example.com wrapi
```

</details>

That's it. Your agents now talk to `:8080` instead of the origin, and every dangerous move needs a human.

## 🔐 The HITL protocol, end to end

**① Agent calls the gateway like a normal API:**

```bash
curl -i -X POST http://localhost:8080/payments \
  -H 'Content-Type: application/json' \
  -d '{"amount": 500, "currency": "USD"}'
```

**② A rule matches → the gateway halts with `403`:**

```json
{
  "error": "human_approval_required",
  "message_for_human": "The agent is attempting to charge 500 via POST /payments. Approving will move real funds. Confirm?",
  "hitl_token": "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9..."
}
```

The `hitl_token` is an HS256 JWT signed with `JWT_SECRET`:

| Claim | Value |
|-------|-------|
| `sub` | `"agent_hitl_request"` |
| `exp` | Unix timestamp, exactly **10 minutes** out |
| `path` | the exact requested path |
| `method` | the exact requested method |
| `body_hash` | **SHA-256** of the raw request body (anti-tampering) |

**③ The agent surfaces `message_for_human` and blocks on a real decision:**

```python
resp = call_gateway("POST", "/payments", body)

if resp.status_code == 403 and resp.json().get("error") == "human_approval_required":
    payload = resp.json()
    if ask_human_for_approval(payload["message_for_human"]):
        # ④ retry the EXACT same request, attaching the token
        resp = call_gateway("POST", "/payments", body,
                            headers={"X-HITL-Approval": payload["hitl_token"]})
    else:
        abort("human denied the action")
```

**⑤ The gateway validates statelessly** — signature, expiry, and that the live request's `path` + `method` + `SHA-256(body)` match the token's claims — then streams it through to `LEGACY_API_URL`. ✅

## 🩹 Agent-friendly errors

Legacy APIs return errors in every shape imaginable — cryptic 4xx JSON, HTML 5xx
stack traces, plain text. Agents burn tokens and retries decoding them. The
gateway rewrites **every** upstream error (and connection failure) into one
consistent, actionable envelope:

```json
{
  "error": "unprocessable_entity",
  "status": 422,
  "message": "amount must be positive",
  "fix": "Validation failed. Inspect `details` and correct the offending fields.",
  "details": { "detail": "amount must be positive", "field": "amount" }
}
```

- **Stable `error` slug** + **actionable `fix`** hint per status (`bad_request`,
  `unauthorized`, `not_found`, `conflict`, `rate_limited`, `upstream_error`, …).
- **4xx `details` are preserved** (validation errors are actionable); **5xx bodies
  are *never* echoed** — no leaking stack traces or internals.
- Connection failures become `{"error":"upstream_unreachable","status":502,...}`.
- On by default; disable with `ERROR_NORMALIZATION=off`.

## 💸 Response projection & token savings

In an agent loop, **input tokens dominate** — and the biggest recurring cost is
the full-fat JSON the agent reads back on every call. `wrapi` trims it.

The compiler emits `response_shapes.json` — a per-operation projection of the
fields an agent actually needs. The gateway applies it on the way out:

```json
[
  { "path": "/users", "method": "GET", "include": ["id", "email"], "max_items": 50 }
]
```

```text
GET /users  →  upstream 1021 bytes  →  gateway 59 bytes  (-95%)
[{"id":1,"email":"u1@ex.com"}, {"id":2,"email":"u2@ex.com"}]   # capped + projected
```

- **`include`** — dot-paths to keep (applied to each record for list endpoints).
- **`max_items`** — cap list length.
- **`list_path`** — when the response wraps the list in an object (e.g.
  `{ "data": [...], "total": N }`), point at the array (`"data"`); the wrapper's
  other fields are preserved. wrapi also auto-detects common keys (`data`,
  `items`, `results`, `records`).
- Operations with **no shape pass through untouched** — projection never removes
  what it isn't sure about.
- On by default; disable with `RESPONSE_PROJECTION=off`.

And at compile time you get the headline number:

```text
[wrapi] agent context: ~87,000 -> ~6,000 tokens (93% smaller)
```

## 🧩 Policy schema

<details>
<summary><b><code>hitl_policy_map.json</code> — the interception rules</b></summary>

```json
[
  {
    "path": "/payments",
    "method": "POST",
    "risk_rules": [
      { "field": "amount", "operator": "GT", "value": 100 }
    ],
    "human_message_template": "The agent is attempting to charge {amount} via {method} {path}. Approving will move real funds. Confirm?"
  }
]
```

- **Matching** — `method` + `path`. Templates (`/users/{id}`) and `*` wildcards compile to anchored regexes at boot.
- **`risk_rules`** — conditions evaluated against the request: **body, query params, and path params merged** (so `{user_id}` in the path is available as `user_id`). Operators: `GT`, `GTE`, `LT`, `LTE`, `EQUALS`, `NOT_EQUALS`, `CONTAINS`, `EXISTS`. `field` supports dot-paths (`customer.tier`). Interception fires only when **all** rules are true (logical AND). An **empty** array means *always intercept*.
- **`human_message_template`** — interpolates `{path}`, `{method}`, and any `{body.field}`.

</details>

### Typed policies — HITL is one of many

The policy map is a **typed policy engine**. Each entry has a `type` (default
`hitl`, for backward compatibility). All implemented types are **stateless** —
they enforce via a signed token or a deterministic transform, so any replica
handles any request. The compiler proposes `hitl` and `pii_redact` policies
automatically (the latter by spotting sensitive fields in response schemas); all
types are also hand-authorable.

| `type` | Phase | What it does |
|--------|-------|--------------|
| `hitl` | ingress | Human-approval gate (signed JWT). *(default)* |
| `audit` | ingress | Emits a structured JSON log line (`audit_fields` selects body fields), then forwards. |
| `dry_run` | ingress | Short-circuits — the request is **never** sent upstream; returns an optional `dry_run_response`. |
| `pii_redact` | egress | Replaces `redact` dot-paths in the response with `[REDACTED]`. |

```json
[
  { "type": "audit",      "method": "POST", "path": "/pay", "audit_fields": ["amount"] },
  { "type": "dry_run",    "method": "POST", "path": "/simulate", "dry_run_response": { "ok": true } },
  { "type": "pii_redact", "method": "GET",  "path": "/users/{id}", "redact": ["ssn", "card.num"] }
]
```

Multiple policies can match one request (e.g. `audit` **and** `hitl`). Precedence:
`audit` → `dry_run` (short-circuit) → `hitl` (gate).

> **Deferred types** — `rate_limit`, `budget_cap`, and `idempotency` are recognized
> but not yet enforced: they need a shared state backend, which would break the
> zero-state guarantee if done naïvely. The gateway logs a warning if it sees one.
> (Tracked in the ROADMAP.)

## 🧬 Composite operations

REST is granular; agents burn turns (and tokens, and error surface) chaining
calls. A **composite** collapses a whole workflow into one agent-callable
endpoint the gateway orchestrates — with value templating between steps and
**rollback-on-failure**. It runs entirely within one request, so statelessness
holds.

`composites.json`:

```json
[
  {
    "name": "onboard_team",
    "method": "POST",
    "path": "/composites/onboard-team",
    "summary": "Create a team and set its budget in one call.",
    "steps": [
      { "id": "team", "method": "POST", "path": "/teams",
        "body": { "name": "{{input.name}}" },
        "rollback": { "method": "DELETE", "path": "/teams/{{steps.team.response.id}}" } },
      { "id": "budget", "method": "PUT",
        "path": "/teams/{{steps.team.response.id}}/budget",
        "body": { "max_budget": "{{input.budget}}" } }
    ],
    "response": { "team_id": "{{steps.team.response.id}}", "status": "onboarded" }
  }
]
```

```text
POST /composites/onboard-team {"name":"eng","budget":500}
  → {"status":"onboarded","team_id":99}          # one call, two upstream steps
```

- **Templating** — `{{input.x}}` (request body) and `{{steps.<id>.response.y}}` /
  `{{steps.<id>.status}}` (prior step results). A full-match expression keeps its
  JSON type; embedded expressions are stringified.
- **Rollback** — if any step fails, completed steps' `rollback` actions run in
  reverse; the caller gets a structured `composite_step_failed` error.
- **Discoverable** — composites are injected into the served `/openapi.json`
  (tagged `x-wrapi-composite`), and still pass through the policy engine (you can
  gate a composite with `hitl`).
- Forwards the caller's `Authorization` header to each step.

## 🌐 Agent spec discovery

The gateway serves the compiler-generated, prompt-optimized spec so an agent discovers exactly the surface it fronts — never the origin's full spec.

| Path | Purpose |
|------|---------|
| `GET /openapi.json` | Conventional discovery path (max tooling compatibility) |
| `GET /.well-known/agent-openapi.json` | Explicit alias |
| `GET /healthz` | Liveness probe (never intercepted or proxied) |

Served **locally** — it works even if the upstream is down, and it intentionally **shadows** the origin's own `/openapi.json`.

## ⚡ Why "stateless"? The JWT *is* the state

The gateway never records that an approval is pending. The 10-minute JWT is minted on interception, held by the agent, and verified purely by **re-hashing the incoming request** and checking the signature + claims. No shared storage means any replica validates any token — horizontal scaling for free, and nothing to reconcile, expire, or clean up.

## 🛠️ Configuration

<details>
<summary><b>Compiler (design time)</b></summary>

> The spec URL and output dir are **CLI arguments** (`wrapi <url> [out]`),
> not env vars. `REMOTE_OPENAPI_URL` is honored only as a fallback when no URL
> argument is given.

| Env | Default | Description |
|-----|---------|-------------|
| `REMOTE_OPENAPI_URL` | *(fallback)* | Used only if the CLI URL argument is omitted |
| `LLM_BASE_URL` | *(required)* | Any OpenAI-compatible base URL |
| `LLM_API_KEY` | *(required)* | Bearer token for that endpoint |
| `LLM_MODEL` | `gpt-4o` | Model id |
| `LLM_MAX_OPS_PER_CALL` | `20` | Operations per batch/LLM call |
| `LLM_CONCURRENCY` | `4` | Max concurrent LLM calls |
| `FETCH_TIMEOUT_MS` | `15000` | Remote spec fetch timeout |

</details>

<details>
<summary><b>Gateway (run time)</b></summary>

| Env | Default | Description |
|-----|---------|-------------|
| `JWT_SECRET` | *(required)* | HMAC signing key for HITL tokens |
| `LEGACY_API_URL` | *(required)* | Upstream base URL to proxy to |
| `GATEWAY_CONFIG_PATH` | `config/hitl_policy_map.json` | Policy map path |
| `AGENT_SPEC_PATH` | `config/agent_openapi.json` | Agent spec path |
| `RESPONSE_SHAPES_PATH` | `config/response_shapes.json` | Response projection path |
| `COMPOSITES_PATH` | `config/composites.json` | Composite operations path |
| `ERROR_NORMALIZATION` | `on` | Normalize upstream errors (`off` to disable) |
| `RESPONSE_PROJECTION` | `on` | Trim 2xx JSON responses (`off` to disable) |
| `LISTEN_ADDR` | `:8080` | Listen address |

</details>

## 📂 Project layout

```
wrapi/
├── package.json           # npm manifest (bin: wrapi) — root so `npx github:…` works
├── compiler/
│   ├── compiler.js        # design-time build pipeline (OpenAI-compatible LLM)
│   └── bin/wrapi.js       # the `wrapi` CLI entry point
├── gateway/
│   ├── main.go            # runtime reverse proxy + stateless HITL middleware
│   ├── go.mod
│   └── config/            # generated by the compiler (+ hand-authored composites)
│       ├── hitl_policy_map.json
│       ├── agent_openapi.json
│       ├── response_shapes.json
│       └── composites.json  (optional, hand-authored)
├── Makefile
└── ROADMAP.md
```

## 🤝 Contributing

PRs, issues, and ideas are welcome. Good first contributions: more rule operators, additional structured-output modes, a reference agent SDK, or example policies for popular APIs.

## 📄 License

Released under the [MIT License](LICENSE).

---

<div align="center">

**Give your AI agents an API they can actually use — cleaner, cheaper, and safe by default.**

</div>
