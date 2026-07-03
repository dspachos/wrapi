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

const SYSTEM_PROMPT = `You are a Principal API Security Architect building a Human-in-the-Loop (HITL) policy compiler.

You are given a SUBSET of a larger OpenAPI (Swagger) specification (a batch of operations plus the components they reference). Analyze ONLY the operations present in this subset. You MUST return a SINGLE JSON object.

JOB 1 — Extract HIGH-RISK endpoints into "hitl_policy_map".
An endpoint is high-risk if invoking it could cause irreversible, costly, destructive, privileged, or externally-visible effects. Examples: money movement / payments / transfers / refunds, deleting or purging resources, sending communications (email/SMS), changing permissions or roles, provisioning/deprovisioning infrastructure, publishing, or bulk mutations.

For each high-risk endpoint present in this subset, emit an object with:
  - "path": the exact OpenAPI path template (e.g. "/payments", "/users/{id}"). Keep {param} placeholders verbatim.
  - "method": the UPPERCASE HTTP method (e.g. "POST", "DELETE").
  - "risk_rules": an array of concrete conditions evaluated against the JSON request body. Each condition is:
        { "field": "<dot.path.into.body>", "operator": "<OP>", "value": <string|number|boolean> }
      Supported operators: "GT", "GTE", "LT", "LTE", "EQUALS", "NOT_EQUALS", "CONTAINS", "EXISTS".
      Derive thresholds from the schema/semantics. Example: a payment endpoint with an "amount" field => { "field": "amount", "operator": "GT", "value": 100 }.
      For inherently destructive endpoints (e.g. DELETE) with no meaningful numeric guard, use a single EXISTS check on the most relevant field, or an empty array [] to mean "ALWAYS require approval".
      The gateway intercepts only when ALL conditions in risk_rules evaluate true (logical AND). An empty array means unconditional interception.
  - "human_message_template": a clear, descriptive sentence shown to a human approver describing the action, its parameters, and its implications. You may interpolate body fields with {field} and also {path} and {method}. Example: "The agent is attempting to charge {amount} via {method} {path}. Approving will move real funds. Confirm?"

Only include genuinely risky endpoints. Read-only GETs and harmless lookups MUST NOT appear.

JOB 2 — Produce "agent_openapi": a stripped, prompt-optimized OpenAPI fragment containing ONLY the absolute semantic information an LLM agent needs for function-calling, for the operations in THIS subset.

For each operation:
  - "operationId": REWRITE it into a clean, self-explanatory tool name for an LLM to select — lowercase snake_case, verb_noun form, derived from what the endpoint DOES, not from the origin's mangled id. E.g. "internal_provision_key_internal_provision_key_post" => "provision_private_ai_key"; "users__user_id__delete" => "delete_user". Make each name unique and unambiguous across the whole API.
  - "summary": REWRITE into a single concise, action-oriented sentence optimized for tool selection: what the endpoint does and when an agent should call it. Drop marketing/boilerplate.
  - Keep: path, method, required parameters/requestBody field names + types + descriptions, and short response semantics.
  - STRIP verbose examples, vendor extensions (x-*), security boilerplate, servers noise, long prose, and anything not needed to decide how to call the endpoint.

Return it as { "paths": { "<path>": { "<method>": {...} } } } — do NOT repeat the info/openapi header (the compiler supplies that). Aim for minimal tokens while preserving meaning.

JOB 3 — Produce "response_shapes": an OPTIONAL array of response projections that shrink verbose responses to only what an agent needs, cutting recurring token cost. For each operation whose successful response is large or list-like, emit:
  - "path": the exact OpenAPI path template (as in JOB 1).
  - "method": the UPPERCASE HTTP method.
  - "include": an array of dot-paths (relative to a single record) the agent needs, e.g. ["id", "email", "team.id"]. For list endpoints these apply to EACH element.
  - "max_items": OPTIONAL integer cap on array length for list endpoints.
Only emit a shape when you are CONFIDENT the omitted fields are not needed to act on the result. If unsure, OMIT the operation entirely — the gateway then passes its response through untouched. Read-only detail GETs and list endpoints are the best candidates; never project write endpoints whose response the caller must inspect in full.

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

function normalizePolicyMap(rawPolicies) {
  const out = [];
  const seen = new Set();
  for (const p of rawPolicies) {
    if (!p || typeof p !== "object") continue;
    if (typeof p.path !== "string" || typeof p.method !== "string") {
      console.warn(`[wrapi] skipping policy with missing path/method: ${JSON.stringify(p)}`);
      continue;
    }
    const method = p.method.toUpperCase();
    const dedupeKey = `${method} ${p.path}`;
    if (seen.has(dedupeKey)) continue; // guard against cross-batch duplicates
    seen.add(dedupeKey);

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
    out.push({
      path: p.path,
      method,
      risk_rules: normRules,
      human_message_template:
        typeof p.human_message_template === "string" && p.human_message_template.trim()
          ? p.human_message_template
          : `The agent is attempting a high-risk operation: {method} {path}. Human approval required.`,
    });
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

    seen.add(key);
    out.push({ path: s.path, method, include, max_items: maxItems });
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
