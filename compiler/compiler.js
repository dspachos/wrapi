#!/usr/bin/env node
/**
 * Stateless Agentic API Gateway Compiler
 * ----------------------------------------
 * Design-time build pipeline:
 *   1. Fetch the target OpenAPI spec from REMOTE_OPENAPI_URL (with timeout /
 *      non-200 / invalid-JSON handling).
 *   2. Split the spec's operations into batches and send each batch to an LLM
 *      over an OpenAI-compatible endpoint, instructing the model to extract
 *      high-risk endpoints and write concrete rule conditions (e.g. payload
 *      `amount` exceeds 100). Batching keeps every call comfortably inside the
 *      model's context window regardless of how large the source spec is.
 *   3. Merge the batch outputs and emit two artifacts consumed by the Go
 *      runtime gateway:
 *        - gateway/config/hitl_policy_map.json  (interception rules)
 *        - gateway/config/agent_openapi.json    (stripped, prompt-optimized spec)
 *
 * The LLM call targets ANY OpenAI-compatible endpoint. Configure via env vars:
 *   LLM_BASE_URL           Base URL of the OpenAI-compatible API
 *   LLM_API_KEY            Bearer token for that endpoint
 *   LLM_MODEL              Model id (default: gpt-4o)
 *   LLM_MAX_OPS_PER_CALL   Operations per batch / LLM call (default: 20)
 *   LLM_CONCURRENCY        Max concurrent LLM calls (default: 4)
 *   FETCH_TIMEOUT_MS       Remote spec fetch timeout (default: 15000)
 */

import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import OpenAI from "openai";

const __dirname = path.dirname(fileURLToPath(import.meta.url));

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

const REMOTE_OPENAPI_URL = process.env.REMOTE_OPENAPI_URL;
const LLM_BASE_URL = process.env.LLM_BASE_URL || process.env.OPENAI_BASE_URL;
const LLM_API_KEY = process.env.LLM_API_KEY || process.env.OPENAI_API_KEY;
const LLM_MODEL = process.env.LLM_MODEL || process.env.OPENAI_MODEL || "gpt-4o";
const MAX_OPS_PER_CALL = Math.max(1, Number(process.env.LLM_MAX_OPS_PER_CALL || 20));
const CONCURRENCY = Math.max(1, Number(process.env.LLM_CONCURRENCY || 4));
const FETCH_TIMEOUT_MS = Number(process.env.FETCH_TIMEOUT_MS || 15000);

// Default output location: gateway/config (so `make run` finds the artifacts).
// Overridable per-invocation via the CLI's [output-dir] argument.
export const DEFAULT_OUTPUT_DIR = path.resolve(__dirname, "..", "gateway", "config");
export const POLICY_MAP_FILENAME = "hitl_policy_map.json";
export const AGENT_SPEC_FILENAME = "agent_openapi.json";
export const RESPONSE_SHAPES_FILENAME = "response_shapes.json";

const HTTP_METHODS = new Set([
  "get", "put", "post", "delete", "options", "head", "patch", "trace",
]);

function die(message) {
  console.error(`\n[wrapi] FATAL: ${message}\n`);
  process.exit(1);
}

// The LLM connection is configured entirely via env; the OpenAPI URL and output
// directory come from the CLI (with env fallback for the URL).
function requireLlmEnv() {
  const missing = [];
  if (!LLM_BASE_URL) missing.push("LLM_BASE_URL");
  if (!LLM_API_KEY) missing.push("LLM_API_KEY");
  if (missing.length) {
    die(`missing required environment variable(s): ${missing.join(", ")}`);
  }
}

// ---------------------------------------------------------------------------
// Step 1 — fetch the remote OpenAPI spec
// ---------------------------------------------------------------------------

async function fetchOpenApiSpec(url) {
  const controller = new AbortController();
  const timer = setTimeout(() => controller.abort(), FETCH_TIMEOUT_MS);

  let res;
  try {
    res = await fetch(url, {
      signal: controller.signal,
      headers: { Accept: "application/json" },
    });
  } catch (err) {
    if (err?.name === "AbortError") {
      die(`connection to ${url} timed out after ${FETCH_TIMEOUT_MS}ms`);
    }
    die(`failed to connect to ${url}: ${err?.message || err}`);
  } finally {
    clearTimeout(timer);
  }

  if (!res.ok) {
    die(`remote spec fetch returned non-200 status: ${res.status} ${res.statusText}`);
  }

  const raw = await res.text();
  let spec;
  try {
    spec = JSON.parse(raw);
  } catch (err) {
    die(`remote spec is not valid JSON: ${err?.message || err}`);
  }

  if (!spec || typeof spec !== "object" || (!spec.paths && !spec.openapi && !spec.swagger)) {
    die("fetched document does not look like an OpenAPI spec (no `paths`/`openapi` field)");
  }

  console.log(`[wrapi] fetched OpenAPI spec (${raw.length} bytes) from ${url}`);
  return spec;
}

// ---------------------------------------------------------------------------
// Spec chunking: split operations into batches + prune $ref components
// ---------------------------------------------------------------------------

/** Flatten spec.paths into a list of individual operations. */
export function flattenOperations(spec) {
  const ops = [];
  const paths = spec.paths || {};
  for (const [p, item] of Object.entries(paths)) {
    if (!item || typeof item !== "object") continue;
    for (const [method, op] of Object.entries(item)) {
      if (HTTP_METHODS.has(method.toLowerCase())) {
        ops.push({ path: p, method: method.toLowerCase(), op, item });
      }
    }
  }
  return ops;
}

/** JSON-pointer token decode (~1 -> /, ~0 -> ~). */
function decodeRefToken(t) {
  return t.replace(/~1/g, "/").replace(/~0/g, "~");
}

/** Recursively collect local `$ref` strings ("#/...") from an object graph. */
function collectRefs(node, out) {
  if (!node || typeof node !== "object") return;
  if (Array.isArray(node)) {
    for (const x of node) collectRefs(x, out);
    return;
  }
  for (const [k, v] of Object.entries(node)) {
    if (k === "$ref" && typeof v === "string" && v.startsWith("#/")) {
      out.push(v);
    } else {
      collectRefs(v, out);
    }
  }
}

/**
 * Given a set of initial refs, resolve the transitive closure of referenced
 * components from the full spec and reconstruct just that subtree. This keeps
 * each batch's payload small: only the schemas actually used by the batch's
 * operations travel with it, not the whole `components`/`definitions` block.
 */
function resolveRefSubset(spec, initialRefs) {
  const result = {};
  const seen = new Set();
  const queue = [...initialRefs];

  while (queue.length) {
    const ref = queue.pop();
    if (seen.has(ref) || !ref.startsWith("#/")) continue;
    seen.add(ref);

    const parts = ref.slice(2).split("/").map(decodeRefToken);
    let node = spec;
    for (const p of parts) node = node?.[p];
    if (node === undefined) continue;

    // Mirror the pointer path into `result`.
    let dst = result;
    for (let i = 0; i < parts.length - 1; i++) {
      dst[parts[i]] = dst[parts[i]] || {};
      dst = dst[parts[i]];
    }
    dst[parts[parts.length - 1]] = node;

    // Follow nested refs.
    collectRefs(node, queue);
  }
  return result;
}

/** Build a self-contained sub-spec for one batch of operations. */
export function buildBatchSpec(spec, batchOps) {
  const paths = {};
  for (const { path: p, method, op, item } of batchOps) {
    paths[p] = paths[p] || {};
    // Preserve shared path-level parameters once per path.
    if (item.parameters && !paths[p].parameters) {
      paths[p].parameters = item.parameters;
    }
    paths[p][method] = op;
  }

  const refs = [];
  collectRefs(paths, refs);
  const refSubset = resolveRefSubset(spec, refs);

  const sub = { paths };
  if (spec.openapi) sub.openapi = spec.openapi;
  if (spec.swagger) sub.swagger = spec.swagger;
  if (spec.info) sub.info = { title: spec.info.title, version: spec.info.version };
  // refSubset yields the top-level sections it touched (components / definitions).
  Object.assign(sub, refSubset);
  return sub;
}

// ---------------------------------------------------------------------------
// Step 2/3 — LLM extraction over an OpenAI-compatible endpoint
// ---------------------------------------------------------------------------

const SYSTEM_PROMPT = `You are wrapi's compiler. You turn a legacy OpenAPI (Swagger) spec into a compact config that makes an existing REST API easy for AI agents to USE, cheap in TOKENS, and SAFE by default.

You are given a SUBSET of a larger spec (a batch of operations plus the components they reference). Analyze ONLY the operations in this subset. NEVER invent paths, fields, parameters, or values that are not present in the subset. Return a SINGLE JSON object.

=== JOB 1 — "hitl_policy_map": a typed list of runtime guardrails ===
Each policy object has:
  - "type": one of "hitl", "pii_redact", "audit". Set it explicitly.
  - "path": the exact OpenAPI path template (keep {param} placeholders verbatim).
  - "method": the UPPERCASE HTTP method.

Emit these types as warranted by the operation:

A) type "hitl" — require a human to approve a dangerous call before it runs.
   Dangerous = irreversible, costly, destructive, privileged, or externally-visible effects: money movement (payments/transfers/refunds), delete/purge, sending communications (email/SMS), changing permissions/roles, provisioning/deprovisioning infra, publishing, or bulk mutations.
   Add:
     - "risk_rules": an array of conditions ANDed together; the gateway intercepts only when ALL are true. An empty array [] means ALWAYS intercept.
         Each rule: { "field": "<dot.path>", "operator": "<OP>", "value": <string|number|boolean> }
         Operators: GT, GTE, LT, LTE, EQUALS, NOT_EQUALS, CONTAINS, EXISTS.
         Fields resolve against a MERGED context of the request body, query params, AND path params — so a path placeholder like {user_id} is available as field "user_id".
         PREFER an empty array (always intercept) for clearly destructive/privileged operations (e.g. any DELETE, role/permission changes). Use a numeric threshold ONLY when the schema implies a real, meaningful boundary — do NOT invent arbitrary numbers. Use EQUALS for a specific dangerous value (e.g. {"field":"is_admin","operator":"EQUALS","value":true}).
     - "human_message_template": ONE clear sentence for a human approver: the action, its key parameters, and its impact. Interpolate {method}, {path}, and any field as {field} (body, query, or path). Example: "The agent is attempting to charge {amount} {currency} via {method} {path}. Confirm?"

B) type "pii_redact" — for read operations whose RESPONSE schema contains sensitive fields; the gateway strips them before the agent sees them.
   Add:
     - "redact": array of dot-paths (relative to a record) to redact. Redact e.g. national IDs (ssn), full card numbers / cvv, passwords, secrets, api keys / tokens, private keys, and personal contact info (phone, home address) WHEN present in the response schema. Do NOT redact identifiers the agent needs (id, resource email) unless clearly sensitive.
   Only emit when the response schema actually contains such fields.

C) type "audit" — for sensitive-but-allowed writes worth logging without blocking (e.g. creating users/teams). Add:
     - "audit_fields": array of field names (body/query/path) to record. Use sparingly.

Read-only lookups that expose nothing sensitive MUST NOT produce a policy.

=== JOB 2 — "agent_openapi": a stripped, function-calling-optimized spec ===
For each operation in the subset:
  - "operationId": REWRITE into a clean, self-explanatory lowercase snake_case verb_noun tool name derived from what the endpoint DOES (not the origin's mangled id). E.g. "users__user_id__delete" => "delete_user". (Global uniqueness is enforced later; just be descriptive.)
  - "summary": ONE concise, action-oriented sentence — what it does and when an agent should call it.
  - "parameters": for each parameter the caller needs, keep name, "in" (path/query/header), "required" (bool), type, a short description, and "enum" if the schema defines allowed values. Enums are important and cheap — always keep them.
  - "requestBody": keep the JSON schema trimmed to property names + types + required list + short descriptions + enums.
  - Keep a short note of the success response (what it returns).
  - STRIP verbose examples, vendor extensions (x-*), security boilerplate, servers, and long prose.
Return as { "paths": { "<path>": { "<method>": {...} } } } — do NOT repeat the info/openapi header. Minimize tokens while preserving meaning.

=== JOB 3 — "response_shapes": OPTIONAL response projections that cut token cost ===
For an operation whose successful response is large or list-like:
  - "path", "method" (UPPERCASE).
  - "include": dot-paths (relative to a SINGLE record) the agent needs. For lists, applied to each element.
  - "max_items": OPTIONAL cap on list length.
  - "list_path": OPTIONAL dot-path to the array when the response WRAPS the list in an object, e.g. "data" for { "data": [...], "total": N }. OMIT it for a bare-array response.
Only emit when you are CONFIDENT the omitted fields are not needed. If unsure, OMIT the operation (the gateway passes it through untouched). Read-only detail GETs and list endpoints are the best candidates; never project a write whose full response the caller must inspect.

Return ONLY a JSON object of the exact shape:
{ "hitl_policy_map": [ ...policy objects... ], "agent_openapi": { "paths": { ... } }, "response_shapes": [ ...shape objects... ] }
Do not wrap it in markdown fences. Do not add commentary.`;

/** Best-effort extraction of a JSON object from an LLM text response. */
function extractJson(text) {
  if (text == null) throw new Error("empty LLM response");
  let s = String(text).trim();

  const fence = s.match(/^```(?:json)?\s*([\s\S]*?)\s*```$/i);
  if (fence) s = fence[1].trim();

  try {
    return JSON.parse(s);
  } catch (_) {
    const first = s.indexOf("{");
    const last = s.lastIndexOf("}");
    if (first !== -1 && last !== -1 && last > first) {
      return JSON.parse(s.slice(first, last + 1));
    }
    throw new Error("could not parse JSON from LLM response");
  }
}

async function compileBatch(client, batchSpec, index, total) {
  const userContent =
    `This is batch ${index + 1} of ${total} from a larger OpenAPI spec.\n\n` +
    "```json\n" +
    JSON.stringify(batchSpec) +
    "\n```";

  const completion = await client.chat.completions.create({
    model: LLM_MODEL,
    temperature: 0,
    // json_object is the most broadly supported structured-output mode across
    // OpenAI-compatible servers; the prompt fully constrains the shape.
    response_format: { type: "json_object" },
    messages: [
      { role: "system", content: SYSTEM_PROMPT },
      { role: "user", content: userContent },
    ],
  });

  const text = completion?.choices?.[0]?.message?.content;
  const parsed = extractJson(text);
  return {
    hitl_policy_map: Array.isArray(parsed.hitl_policy_map) ? parsed.hitl_policy_map : [],
    agent_openapi:
      parsed.agent_openapi && typeof parsed.agent_openapi === "object"
        ? parsed.agent_openapi
        : { paths: {} },
    response_shapes: Array.isArray(parsed.response_shapes) ? parsed.response_shapes : [],
  };
}

// ---------------------------------------------------------------------------
// Concurrency pool
// ---------------------------------------------------------------------------

async function runPool(items, worker, concurrency) {
  const results = new Array(items.length);
  let next = 0;
  async function runner() {
    while (true) {
      const i = next++;
      if (i >= items.length) return;
      results[i] = await worker(items[i], i);
    }
  }
  const workers = Array.from({ length: Math.min(concurrency, items.length) }, runner);
  await Promise.all(workers);
  return results;
}

// ---------------------------------------------------------------------------
// Merge + normalize
// ---------------------------------------------------------------------------

const VALID_OPERATORS = new Set([
  "GT", "GTE", "LT", "LTE", "EQUALS", "NOT_EQUALS", "CONTAINS", "EXISTS",
]);

// Policy types the gateway enforces today (all stateless).
const VALID_POLICY_TYPES = new Set(["hitl", "audit", "pii_redact", "dry_run"]);

export function normalizePolicyMap(rawPolicies) {
  const out = [];
  const seen = new Set();
  for (const p of rawPolicies) {
    if (!p || typeof p !== "object") continue;
    if (typeof p.path !== "string" || typeof p.method !== "string") {
      console.warn(`[wrapi] skipping policy with missing path/method: ${JSON.stringify(p)}`);
      continue;
    }
    const method = p.method.toUpperCase();
    const type =
      typeof p.type === "string" && p.type.trim() ? p.type.toLowerCase() : "hitl";
    if (!VALID_POLICY_TYPES.has(type)) {
      console.warn(`[wrapi] dropping policy with unsupported type "${p.type}" (${method} ${p.path})`);
      continue;
    }
    // Dedupe by type+method+path so an endpoint can carry multiple policy types
    // (e.g. audit AND hitl) without one clobbering the other.
    const dedupeKey = `${type} ${method} ${p.path}`;
    if (seen.has(dedupeKey)) continue;

    let entry;
    if (type === "hitl") {
      const rules = Array.isArray(p.risk_rules) ? p.risk_rules : [];
      const normRules = [];
      for (const r of rules) {
        if (!r || typeof r.field !== "string" || typeof r.operator !== "string") continue;
        const op = r.operator.toUpperCase();
        if (!VALID_OPERATORS.has(op)) {
          console.warn(`[wrapi] dropping rule with unknown operator "${r.operator}"`);
          continue;
        }
        normRules.push({ field: r.field, operator: op, value: r.value ?? null });
      }
      entry = {
        type,
        path: p.path,
        method,
        risk_rules: normRules,
        human_message_template:
          typeof p.human_message_template === "string" && p.human_message_template.trim()
            ? p.human_message_template
            : `The agent is attempting a high-risk operation: {method} {path}. Human approval required.`,
      };
    } else if (type === "pii_redact") {
      const redact = Array.isArray(p.redact)
        ? p.redact.filter((f) => typeof f === "string" && f.trim())
        : [];
      if (redact.length === 0) {
        console.warn(`[wrapi] dropping pii_redact policy with no fields (${method} ${p.path})`);
        continue;
      }
      entry = { type, path: p.path, method, redact };
    } else if (type === "audit") {
      const auditFields = Array.isArray(p.audit_fields)
        ? p.audit_fields.filter((f) => typeof f === "string" && f.trim())
        : [];
      entry = { type, path: p.path, method, audit_fields: auditFields };
    } else {
      // dry_run
      entry = { type, path: p.path, method };
      if (p.dry_run_response !== undefined) entry.dry_run_response = p.dry_run_response;
    }

    seen.add(dedupeKey);
    out.push(entry);
  }
  return out;
}

/** Convert an arbitrary string into a clean snake_case identifier. */
export function toSnakeCase(s) {
  return String(s)
    .replace(/([A-Z]+)([A-Z][a-z])/g, "$1_$2") // ACRONYMWord -> ACRONYM_Word (AIKey -> AI_Key)
    .replace(/([a-z0-9])([A-Z])/g, "$1_$2") // camelCase -> camel_Case
    .replace(/[^a-zA-Z0-9]+/g, "_") // non-alnum -> _
    .replace(/_+/g, "_")
    .replace(/^_|_$/g, "")
    .toLowerCase();
}

/** Derive a fallback tool name from method + path, e.g. delete /users/{id} -> delete_users_by_id. */
export function deriveOperationId(method, p) {
  const parts = String(p)
    .split("/")
    .filter(Boolean)
    .map((seg) => {
      const m = seg.match(/^\{(.+)\}$/);
      return m ? `by_${m[1]}` : seg;
    });
  return toSnakeCase([method, ...parts].join("_")) || toSnakeCase(String(method));
}

/**
 * Ensure every operation in the merged agent spec has a clean, UNIQUE snake_case
 * operationId (the agent's tool name). The LLM is asked to do this in JOB 2, but
 * we enforce it deterministically so tool names are always valid and collision-free:
 *   - missing/blank ids are derived from method + path
 *   - non-snake_case ids are normalized
 *   - cross-batch collisions get a numeric suffix (_2, _3, ...)
 */
export function normalizeAgentSpec(spec) {
  const seen = new Set();
  const paths = spec.paths || {};
  for (const [p, methods] of Object.entries(paths)) {
    if (!methods || typeof methods !== "object") continue;
    for (const [method, op] of Object.entries(methods)) {
      if (!HTTP_METHODS.has(method.toLowerCase())) continue; // skip path-level parameters, etc.
      if (!op || typeof op !== "object") continue;

      let base =
        typeof op.operationId === "string" && op.operationId.trim()
          ? toSnakeCase(op.operationId)
          : deriveOperationId(method, p);
      if (!base) base = deriveOperationId(method, p);

      let unique = base;
      let n = 2;
      while (seen.has(unique)) unique = `${base}_${n++}`;
      seen.add(unique);
      op.operationId = unique;
    }
  }
  return spec;
}

/**
 * Normalize + de-duplicate response-shape projections across batches.
 * Keeps only shapes with a valid path/method and a non-empty include list or a
 * positive max_items (a shape that does neither would be a no-op).
 */
export function normalizeResponseShapes(rawShapes) {
  const out = [];
  const seen = new Set();
  for (const s of rawShapes) {
    if (!s || typeof s !== "object") continue;
    if (typeof s.path !== "string" || typeof s.method !== "string") continue;
    const method = s.method.toUpperCase();
    const key = `${method} ${s.path}`;
    if (seen.has(key)) continue;

    const include = Array.isArray(s.include)
      ? s.include.filter((f) => typeof f === "string" && f.trim())
      : [];
    const maxItems = Number.isInteger(s.max_items) && s.max_items > 0 ? s.max_items : 0;
    if (include.length === 0 && maxItems === 0) continue; // no-op shape

    const entry = { path: s.path, method, include, max_items: maxItems };
    if (typeof s.list_path === "string" && s.list_path.trim()) {
      entry.list_path = s.list_path.trim();
    }

    seen.add(key);
    out.push(entry);
  }
  return out;
}

/** Rough token estimate for logging cost savings (~4 chars/token heuristic). */
export function estimateTokens(obj) {
  const s = typeof obj === "string" ? obj : JSON.stringify(obj);
  return Math.ceil(s.length / 4);
}

/** Merge per-batch agent_openapi fragments into one spec. */
function mergeAgentSpec(spec, batchResults) {
  const merged = {
    paths: {},
  };
  if (spec.openapi) merged.openapi = spec.openapi;
  if (spec.swagger) merged.swagger = spec.swagger;
  if (spec.info) merged.info = { title: spec.info.title, version: spec.info.version };

  for (const r of batchResults) {
    const frag = r.agent_openapi || {};
    const paths = frag.paths || {};
    for (const [p, methods] of Object.entries(paths)) {
      merged.paths[p] = { ...(merged.paths[p] || {}), ...methods };
    }
    // Carry through any stripped components the model chose to keep.
    for (const section of ["components", "definitions"]) {
      if (frag[section] && typeof frag[section] === "object") {
        merged[section] = { ...(merged[section] || {}), ...frag[section] };
      }
    }
  }
  return merged;
}

// ---------------------------------------------------------------------------
// Main
// ---------------------------------------------------------------------------

/**
 * Run the full design-time pipeline.
 *
 * @param {object} [opts]
 * @param {string} [opts.remoteUrl]  OpenAPI spec URL (falls back to $REMOTE_OPENAPI_URL).
 * @param {string} [opts.outputDir]  Directory for the generated files (default: DEFAULT_OUTPUT_DIR).
 * @returns {Promise<{policyMapPath: string, agentSpecPath: string, policyCount: number, pathCount: number}>}
 */
export async function compile({ remoteUrl, outputDir } = {}) {
  requireLlmEnv();

  const specUrl = remoteUrl || REMOTE_OPENAPI_URL;
  if (!specUrl) {
    die("no OpenAPI URL provided (pass it as the first CLI argument or set REMOTE_OPENAPI_URL)");
  }

  const outDir = outputDir || DEFAULT_OUTPUT_DIR;
  const policyMapPath = path.join(outDir, POLICY_MAP_FILENAME);
  const agentSpecPath = path.join(outDir, AGENT_SPEC_FILENAME);
  const responseShapesPath = path.join(outDir, RESPONSE_SHAPES_FILENAME);

  const spec = await fetchOpenApiSpec(specUrl);

  const ops = flattenOperations(spec);
  if (ops.length === 0) {
    console.warn("[wrapi] spec contains no operations; emitting empty artifacts.");
  }
  console.log(`[wrapi] found ${ops.length} operation(s)`);

  // Chunk operations into batches to stay inside the model context window.
  const batches = [];
  for (let i = 0; i < ops.length; i += MAX_OPS_PER_CALL) {
    batches.push(ops.slice(i, i + MAX_OPS_PER_CALL));
  }
  console.log(
    `[wrapi] split into ${batches.length} batch(es) of up to ${MAX_OPS_PER_CALL} ops ` +
      `(concurrency ${CONCURRENCY}) -> model "${LLM_MODEL}" at ${LLM_BASE_URL}`
  );

  const client = new OpenAI({ baseURL: LLM_BASE_URL, apiKey: LLM_API_KEY });

  const batchSpecs = batches.map((b) => buildBatchSpec(spec, b));
  const total = batchSpecs.length;

  const batchResults = await runPool(
    batchSpecs,
    async (batchSpec, i) => {
      console.log(`[wrapi] -> batch ${i + 1}/${total} (${batches[i].length} ops)`);
      try {
        const r = await compileBatch(client, batchSpec, i, total);
        console.log(`[wrapi] <- batch ${i + 1}/${total}: ${r.hitl_policy_map.length} risky endpoint(s)`);
        return r;
      } catch (err) {
        die(`batch ${i + 1}/${total} failed: ${err?.message || err}`);
      }
    },
    CONCURRENCY
  );

  const allPolicies = batchResults.flatMap((r) => r.hitl_policy_map);
  const policyMap = normalizePolicyMap(allPolicies);
  const agentSpec = normalizeAgentSpec(mergeAgentSpec(spec, batchResults));
  const responseShapes = normalizeResponseShapes(
    batchResults.flatMap((r) => r.response_shapes || [])
  );

  fs.mkdirSync(outDir, { recursive: true });
  fs.writeFileSync(policyMapPath, JSON.stringify(policyMap, null, 2) + "\n", "utf8");
  fs.writeFileSync(agentSpecPath, JSON.stringify(agentSpec, null, 2) + "\n", "utf8");
  fs.writeFileSync(responseShapesPath, JSON.stringify(responseShapes, null, 2) + "\n", "utf8");

  const pathCount = Object.keys(agentSpec.paths).length;
  console.log(
    `[wrapi] wrote ${policyMap.length} HITL polic${policyMap.length === 1 ? "y" : "ies"} -> ${policyMapPath}`
  );
  console.log(`[wrapi] wrote agent spec (${pathCount} paths) -> ${agentSpecPath}`);
  console.log(`[wrapi] wrote ${responseShapes.length} response shape(s) -> ${responseShapesPath}`);

  // Token accounting: the agent's context cost, origin spec vs. stripped spec.
  const rawTokens = estimateTokens(spec);
  const agentTokens = estimateTokens(agentSpec);
  const pct = rawTokens ? Math.round((1 - agentTokens / rawTokens) * 100) : 0;
  console.log(
    `[wrapi] agent context: ~${rawTokens.toLocaleString()} -> ~${agentTokens.toLocaleString()} tokens (${pct}% smaller)`
  );
  console.log("[wrapi] build complete.");

  return {
    policyMapPath,
    agentSpecPath,
    responseShapesPath,
    policyCount: policyMap.length,
    pathCount,
    shapeCount: responseShapes.length,
    rawTokens,
    agentTokens,
    tokenReductionPct: pct,
  };
}

// Back-compat: `node compiler.js` still runs a full compile using env vars only.
// The primary entry point is the CLI at compiler/bin/wrapi.js (command: `wrapi`).
const invokedDirectly =
  process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (invokedDirectly) {
  compile().catch((err) => {
    die(err?.stack || err?.message || String(err));
  });
}
