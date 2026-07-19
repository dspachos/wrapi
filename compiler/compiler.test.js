import { test } from "node:test";
import assert from "node:assert/strict";
import {
  flattenOperations,
  buildBatchSpec,
  toSnakeCase,
  deriveOperationId,
  normalizeAgentSpec,
  normalizeResponseShapes,
  normalizePolicyMap,
  estimateTokens,
  parsePositiveInteger,
} from "./compiler.js";

test("parsePositiveInteger rejects malformed compiler limits", () => {
  assert.equal(parsePositiveInteger("25", 10), 25);
  assert.equal(parsePositiveInteger("not-a-number", 10), 10);
  assert.equal(parsePositiveInteger("0", 10), 10);
  assert.equal(parsePositiveInteger("-3", 10), 10);
  assert.equal(parsePositiveInteger("2.5", 10), 10);
  assert.equal(parsePositiveInteger(undefined, 10), 10);
});

test("flattenOperations extracts one entry per method", () => {
  const spec = {
    paths: {
      "/users": {
        get: { operationId: "listUsers" },
        post: { operationId: "createUser" },
      },
      "/users/{id}": {
        delete: { operationId: "deleteUser" },
        parameters: [{ name: "id", in: "path" }], // not an HTTP method
      },
    },
  };
  const ops = flattenOperations(spec);
  assert.equal(ops.length, 3);
  const keys = ops.map((o) => `${o.method} ${o.path}`).sort();
  assert.deepEqual(keys, ["delete /users/{id}", "get /users", "post /users"]);
});

test("flattenOperations tolerates an empty/missing paths object", () => {
  assert.deepEqual(flattenOperations({}), []);
  assert.deepEqual(flattenOperations({ paths: {} }), []);
});

test("buildBatchSpec resolves the transitive $ref closure and prunes the rest", () => {
  const spec = {
    openapi: "3.0.0",
    info: { title: "T", version: "1" },
    paths: {
      "/pay": {
        post: {
          operationId: "pay",
          requestBody: {
            content: {
              "application/json": {
                schema: { $ref: "#/components/schemas/Payment" },
              },
            },
          },
        },
      },
    },
    components: {
      schemas: {
        Payment: {
          type: "object",
          properties: { card: { $ref: "#/components/schemas/Card" } },
        },
        Card: { type: "object", properties: { pan: { type: "string" } } },
        Unused: { type: "object" }, // must be pruned
      },
    },
  };

  const ops = flattenOperations(spec);
  const sub = buildBatchSpec(spec, ops);

  // Header carried through.
  assert.equal(sub.openapi, "3.0.0");
  assert.equal(sub.info.title, "T");

  // Referenced schemas present; unused one pruned.
  assert.ok(sub.components.schemas.Payment, "Payment should be included");
  assert.ok(sub.components.schemas.Card, "transitively-referenced Card should be included");
  assert.equal(sub.components.schemas.Unused, undefined, "Unused should be pruned");

  // Operation preserved.
  assert.ok(sub.paths["/pay"].post, "operation should be preserved");
});

test("buildBatchSpec preserves shared path-level parameters", () => {
  const spec = {
    paths: {
      "/users/{id}": {
        parameters: [{ name: "id", in: "path", required: true }],
        get: { operationId: "getUser" },
      },
    },
  };
  const sub = buildBatchSpec(spec, flattenOperations(spec));
  assert.equal(sub.paths["/users/{id}"].parameters[0].name, "id");
  assert.ok(sub.paths["/users/{id}"].get);
});

test("toSnakeCase normalizes mixed input", () => {
  assert.equal(toSnakeCase("provisionPrivateAIKey"), "provision_private_ai_key");
  assert.equal(toSnakeCase("internal_provision_key_POST"), "internal_provision_key_post");
  assert.equal(toSnakeCase("Create User / Account!!"), "create_user_account");
  assert.equal(toSnakeCase("--already_snake--"), "already_snake");
});

test("deriveOperationId builds a name from method + path", () => {
  assert.equal(deriveOperationId("delete", "/users/{user_id}"), "delete_users_by_user_id");
  assert.equal(deriveOperationId("post", "/teams/{team_id}/merge"), "post_teams_by_team_id_merge");
  assert.equal(deriveOperationId("get", "/"), "get");
});

test("normalizeAgentSpec assigns clean, unique operationIds", () => {
  const spec = {
    paths: {
      "/users/{id}": {
        get: { operationId: "getUserById" }, // -> snake_case
        delete: {}, // -> derived
      },
      "/accounts/{id}": {
        get: { operationId: "getUserById" }, // collides -> suffixed
      },
      "/health": {
        get: {},
        parameters: [{ name: "x" }], // must be ignored
      },
    },
  };
  normalizeAgentSpec(spec);
  assert.equal(spec.paths["/users/{id}"].get.operationId, "get_user_by_id");
  assert.equal(spec.paths["/users/{id}"].delete.operationId, "delete_users_by_id");
  assert.equal(spec.paths["/accounts/{id}"].get.operationId, "get_user_by_id_2");
  assert.equal(spec.paths["/health"].get.operationId, "get_health");
  // path-level parameters array is left untouched
  assert.ok(Array.isArray(spec.paths["/health"].parameters));
});

test("normalizeResponseShapes validates, dedupes, and drops no-ops", () => {
  const shapes = normalizeResponseShapes([
    { path: "/users", method: "get", include: ["id", "email"], max_items: 50 },
    { path: "/users", method: "GET", include: ["id"] }, // duplicate -> dropped
    { path: "/x", method: "GET" }, // no include + no max_items -> no-op dropped
    { path: "/y", method: "GET", max_items: 10 }, // cap-only shape kept
    { method: "GET" }, // missing path -> dropped
    { path: "/z", method: "get", include: ["a", 5, ""] }, // non-string entries filtered
  ]);
  assert.equal(shapes.length, 3);
  assert.deepEqual(shapes[0], { path: "/users", method: "GET", include: ["id", "email"], max_items: 50 });
  assert.deepEqual(shapes[1], { path: "/y", method: "GET", include: [], max_items: 10 });
  assert.deepEqual(shapes[2], { path: "/z", method: "GET", include: ["a"], max_items: 0 });
});

test("normalizePolicyMap defaults type to hitl and normalizes rules", () => {
  const out = normalizePolicyMap([
    {
      method: "post",
      path: "/payments",
      risk_rules: [{ field: "amount", operator: "gt", value: 100 }],
      human_message_template: "Charge {amount}?",
    },
  ]);
  assert.equal(out.length, 1);
  assert.equal(out[0].type, "hitl");
  assert.equal(out[0].method, "POST");
  assert.deepEqual(out[0].risk_rules, [{ field: "amount", operator: "GT", value: 100 }]);
});

test("normalizePolicyMap keeps multiple types on the same endpoint", () => {
  const out = normalizePolicyMap([
    { type: "audit", method: "POST", path: "/teams", audit_fields: ["name"] },
    { type: "hitl", method: "POST", path: "/teams", risk_rules: [] },
    { type: "hitl", method: "POST", path: "/teams", risk_rules: [] }, // dup -> dropped
  ]);
  assert.equal(out.length, 2);
  assert.deepEqual(out.map((p) => p.type).sort(), ["audit", "hitl"]);
});

test("normalizePolicyMap handles pii_redact / dry_run and drops bad ones", () => {
  const out = normalizePolicyMap([
    { type: "pii_redact", method: "GET", path: "/users/{id}", redact: ["ssn", 5, ""] },
    { type: "pii_redact", method: "GET", path: "/x", redact: [] }, // no fields -> dropped
    { type: "dry_run", method: "POST", path: "/emails", dry_run_response: { sent: false } },
    { type: "bogus", method: "GET", path: "/y" }, // unsupported -> dropped
  ]);
  assert.equal(out.length, 2);
  const redact = out.find((p) => p.type === "pii_redact");
  assert.deepEqual(redact.redact, ["ssn"]);
  const dry = out.find((p) => p.type === "dry_run");
  assert.deepEqual(dry.dry_run_response, { sent: false });
});

test("normalizeResponseShapes carries list_path for wrapped lists", () => {
  const out = normalizeResponseShapes([
    { path: "/users", method: "GET", include: ["id"], list_path: "data" },
  ]);
  assert.equal(out.length, 1);
  assert.equal(out[0].list_path, "data");
});

test("estimateTokens is a stable ~chars/4 heuristic", () => {
  assert.equal(estimateTokens("aaaa"), 1);
  assert.equal(estimateTokens("a".repeat(400)), 100);
  assert.equal(estimateTokens({ a: 1 }), Math.ceil(JSON.stringify({ a: 1 }).length / 4));
});
