#!/usr/bin/env node
/**
 * wrapi — CLI for the design-time compiler.
 *
 * Compiles a remote OpenAPI spec into the two artifacts the Go gateway consumes:
 *   <output-dir>/hitl_policy_map.json
 *   <output-dir>/agent_openapi.json
 *
 * The OpenAPI URL (positional) and an optional output directory are the only
 * inputs. All LLM_* configuration is read from the environment.
 *
 * Usage:
 *   wrapi <remote-openapi-url> [output-dir]
 *   wrapi --url <url> --out <dir>
 */

import { parseArgs } from "node:util";
import path from "node:path";
import { createRequire } from "node:module";
import { compile, DEFAULT_OUTPUT_DIR } from "../compiler.js";

const require = createRequire(import.meta.url);
const pkg = require("../../package.json");

const HELP = `wrapi — compile an OpenAPI spec into an agent-ready gateway config

Usage:
  wrapi <remote-openapi-url> [output-dir]
  wrapi --url <url> --out <dir>

Arguments:
  remote-openapi-url   URL of the OpenAPI spec to compile.
                       Falls back to $REMOTE_OPENAPI_URL if omitted.
  output-dir           Directory for the generated files.
                       Default: ${DEFAULT_OUTPUT_DIR}

Options:
  -o, --out <dir>      Output directory (same as the [output-dir] positional).
  -u, --url <url>      OpenAPI URL (same as the <remote-openapi-url> positional).
  -h, --help           Show this help.
  -v, --version        Print the version.

Environment (LLM configuration — read from env, never the CLI):
  LLM_BASE_URL         (required) OpenAI-compatible base URL
  LLM_API_KEY          (required) bearer token
  LLM_MODEL            model id (default: gpt-4o)
  LLM_MAX_OPS_PER_CALL operations per batch (default: 20)
  LLM_CONCURRENCY      concurrent LLM calls (default: 4)
  FETCH_TIMEOUT_MS     remote fetch timeout ms (default: 15000)

Examples:
  wrapi https://api.example.com/openapi.json
  wrapi https://api.example.com/openapi.json ./out
  wrapi --url https://api.example.com/openapi.json --out ./out
`;

function fail(message, code = 2) {
  console.error(`\n[wrapi] ${message}\n`);
  console.error(HELP);
  process.exit(code);
}

function main() {
  let values, positionals;
  try {
    ({ values, positionals } = parseArgs({
      allowPositionals: true,
      options: {
        out: { type: "string", short: "o" },
        url: { type: "string", short: "u" },
        help: { type: "boolean", short: "h" },
        version: { type: "boolean", short: "v" },
      },
    }));
  } catch (err) {
    fail(err.message);
    return;
  }

  if (values.help) {
    console.log(HELP);
    process.exit(0);
  }
  if (values.version) {
    console.log(pkg.version);
    process.exit(0);
  }

  const remoteUrl = values.url || positionals[0] || process.env.REMOTE_OPENAPI_URL;
  if (!remoteUrl) {
    fail("no OpenAPI URL provided — pass it as the first argument or set $REMOTE_OPENAPI_URL.");
    return;
  }

  // Resolve the output dir relative to the caller's cwd; undefined -> compile()
  // falls back to DEFAULT_OUTPUT_DIR (gateway/config).
  const outArg = values.out ?? positionals[1];
  const outputDir = outArg !== undefined ? path.resolve(outArg) : undefined;

  compile({ remoteUrl, outputDir }).catch((err) => {
    console.error(`\n[wrapi] FATAL: ${err?.stack || err?.message || err}\n`);
    process.exit(1);
  });
}

main();
