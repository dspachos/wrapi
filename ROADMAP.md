# Roadmap

> The plan for turning `wrapi` from a clever HITL proxy into **the default
> way to expose any legacy REST API to AI agents.**

---

## 1. Vision

**Most APIs were built for human developers, not agents.** They have cryptic
`operationId`s, granular endpoints that force multi-call workflows, verbose specs
that blow up context windows, and raw responses that bleed tokens on every turn.
Agents *can* call them — badly, expensively, and dangerously.

`wrapi` is a **compiler + runtime that transforms a legacy REST API into
an agent-native interface.** You point it at an existing `openapi.json`; it produces
a hardened gateway that is:

- **Understandable** — clean tool names, agent-optimized descriptions, workflow-level operations.
- **Cheap** — minimal specs *and* trimmed responses, with the token savings measured and reported.
- **Safe** — a policy engine (human-in-the-loop, budgets, rate limits, idempotency, audit) enforced statelessly.

Human-in-the-loop is **one policy**, not the product.

## 2. The three gaps we close

| Gap | Symptom for an agent | Our answer |
|-----|----------------------|------------|
| 🧠 **Comprehension** | Picks the wrong tool; can't chain calls; fails on auth. | Semantic renaming, LLM-authored descriptions, composite/intent endpoints, error normalization. |
| 💸 **Cost** | Pays tokens for a huge spec *and* every bloated response, on every loop. | Stripped spec (one-time) **+ response projection (recurring)** + pagination guards, with a published savings metric. |
| 🛡️ **Control** | Hallucinates, retries, acts irreversibly. | Typed policy engine: HITL, budget caps, rate limits, idempotency, dry-run, PII redaction, audit — all stateless. |

**The cost insight that drives the design:** in an agent loop, *input tokens
dominate*. Shrinking the spec is a one-time win; shrinking every *response* the
agent reads back is the recurring win. The gateway sits in the one place that can
do both.

## 3. Product principles

1. **Design-time LLM, run-time determinism.** The LLM only runs in the compiler.
   The gateway is pure, fast, and predictable — no model calls on the hot path.
2. **Everything the compiler emits is human-reviewable and version-controlled.**
   Outputs are plain JSON, diffable, and safe to edit by hand.
3. **Zero shared runtime state.** State lives in signed, short-lived tokens.
   Any replica handles any request. (Already true for HITL; keep it true for
   everything.)
4. **Measure the value.** Every release reports token/turn savings on a reference
   spec. The number *is* the marketing.
5. **HTTP-native now.** Agents consume the layer as clean, enriched REST plus a
   served function-calling spec (`/openapi.json`). MCP is a later *transport* over
   the same core — not the foundation.

## 4. Architecture: today → target

**Today**
```
compiler.js → hitl_policy_map.json + agent_openapi.json → Go gateway (HITL + proxy)
```

**Target**
```
                       ┌─────────────── compiler (design time) ───────────────┐
 REMOTE_OPENAPI_URL ─▶  fetch → flatten → LLM passes:                          │
                       │   • risk/policy extraction                            │
                       │   • semantic renaming + description rewrite           │
                       │   • response projection synthesis                     │
                       │   • composite/intent operation proposals             │
                       └──────────────────────┬───────────────────────────────┘
                                               ▼
                    artifacts/ (versioned, reviewable JSON)
                      • policy_map.json        (typed policies)
                      • tool_manifest.json     (renamed, described tools)
                      • response_shapes.json   (per-op projections)
                      • composites.json        (workflow operations)
                                               │
                       ┌─────────────── gateway (run time) ───────────────────┐
   Agent (HTTP) ─▶     │  HTTP proxy ─▶ policy engine ─▶ request reshape ─▶     │──▶ LEGACY_API_URL
                       │  GET /openapi.json (enriched spec)                     │
                       │                    ◀─ response projection ◀───────────│◀──
                       └──────────────────────────────────────────────────────┘
```

## 5. Phased plan

Each phase ships something usable and moves a headline metric.

### Phase 0 — Foundation & framing *(prep)* — ✅ done
- [x] `.gitignore`, repo init.
- [x] `LICENSE` (MIT, © dspachos).
- [x] `.env.example` for both compiler and gateway.
- [x] Test scaffolding (Go: `go test`; compiler: node test runner) + `make test`.
- [ ] Reframe README headline around the token metric — deferred to pair with Phase 2.

### Phase 1 — Comprehension: make agents *good* at the API — ✅ done
- [x] **Semantic tool naming** — LLM rewrites `operationId`s into clean tool names; `normalizeAgentSpec` enforces unique snake_case deterministically.
- [x] **Description rewriting** — tool-selection-optimized summaries (LLM JOB 2).
- [x] **Error normalization** — gateway maps upstream 4xx/5xx + connection errors into a structured `{error, status, message, fix, details}` envelope; 5xx bodies never leaked. Toggle: `ERROR_NORMALIZATION`.
- **Metric:** tool-selection accuracy on a fixed eval; retry rate down. *(eval harness lands in Phase 5.)*

### Phase 2 — Cost: attack the recurring token bill — ✅ done
- [x] **Response projection** — compiler emits `response_shapes.json` (per-op `include` + `max_items`); gateway strips the rest on egress (`projectResponse`). No-shape ops pass through untouched.
- [x] **Pagination / list guards** — `max_items` caps list length per shape.
- [x] **Token accounting** — compiler prints `~X -> ~Y tokens (Z% smaller)`; gateway logs per-call `N -> M bytes (-P%)`.
- **Metric:** ⭐ *"context X→Y tokens (Z% smaller), payload −N%"* — now emitted at compile + request time. (Demo: agent context 79% smaller; a fat list response −95%.)

### Phase 3 — Control: HITL becomes one policy of many — ✅ (stateless types done)
- [x] Typed policy engine — each entry has a `type`, default `hitl` (backward compatible with the untyped map). Multiple policies can match one request.
- [x] Stateless types implemented: `hitl` (JWT gate), `audit` (structured log), `dry_run` (short-circuit), `pii_redact` (egress redaction).
- [x] Keep every policy stateless — all four enforce via signed token or deterministic transform.
- [ ] **Deferred (need a shared state backend):** `rate_limit`, `budget_cap`, `idempotency`. Recognized at load with a warning; enforcement is a future "stateful policies" phase.
- **Metric:** number of policy types; % of destructive ops covered by default.

### Phase 4 — Composites: collapse the chatter — ✅ done
- [x] **Composite/intent operations** — hand-authored `composites.json` exposes workflow-level endpoints (`onboard_team` = create team + set budget) as new gateway routes, injected into the served `/openapi.json`.
- [x] Gateway orchestrates the steps with value templating (`{{input.x}}`, `{{steps.<id>.response.y}}`) and **rollback-on-failure** (compensating actions in reverse). Runs within one request → stays stateless.
- [ ] *Future:* compiler auto-proposes composites from the spec (currently hand-authored).
- **Metric:** avg agent turns per completed task (a 2-call workflow → 1 call).

### Phase 5 — Polish & adoption — ✅ (core done)
- [x] CI — GitHub Actions runs node + go tests, `gofmt` check, binary + Docker build.
- [x] Container image — multi-stage `Dockerfile` (distroless), `make docker`.
- [x] Example configs — `examples/` (typed policies + composite) with a local demo.
- [x] Token benchmark — `scripts/bench-tokens.mjs` / `make bench`.
- [x] Contributor docs — `CONTRIBUTING.md`.
- [ ] *Future:* eval harness for tool-selection accuracy; example configs for popular public APIs (Stripe/GitHub); docs site; releases.

### Later (deferred) — MCP transport
Not part of the core layer. Once the HTTP agent-native surface is solid, we can add
an MCP front door over the **same** policy + reshaping core: compiler emits an MCP
tool manifest, gateway serves MCP (stdio + Streamable HTTP), one-command install for
agent hosts. Deferred by choice — the value is the layer, not the protocol.

## 6. Success metrics (the scoreboard)

| Metric | Why it matters | Where it shows up |
|--------|----------------|-------------------|
| Agent context tokens (before → after) | The viral stat | README headline |
| Avg response payload reduction | Recurring cost | Phase 2 |
| Tool-selection accuracy | Comprehension | Phase 1 eval |
| Agent turns per task | Chattiness | Phase 5 |
| Retry rate | Error handling | Phase 1 |
| % destructive ops guarded | Safety | Phase 4 |

## 7. Non-goals (for now)

- Running the LLM at request time (kills determinism, latency, and cost).
- Being a general API management platform (rate plans, monetization, dev portals).
- Replacing the origin API's auth model — we wrap it, we don't reinvent it.
- Supporting non-OpenAPI sources in v1 (GraphQL, gRPC come later).

---

*This roadmap is the source of truth for the reframe. The README, code, and issues
should track back to it.*
