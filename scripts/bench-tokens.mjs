#!/usr/bin/env node
/**
 * bench-tokens — report the agent-context token reduction for a compiled spec.
 *
 * Usage:
 *   node scripts/bench-tokens.mjs <raw-openapi.json> <agent_openapi.json>
 *
 * Uses the same ~4-chars/token heuristic the compiler reports at build time.
 */
import fs from "node:fs";
import { estimateTokens } from "../compiler/compiler.js";

const [rawPath, agentPath] = process.argv.slice(2);
if (!rawPath || !agentPath) {
  console.error("usage: node scripts/bench-tokens.mjs <raw-openapi.json> <agent_openapi.json>");
  process.exit(2);
}

function load(p) {
  try {
    return JSON.parse(fs.readFileSync(p, "utf8"));
  } catch (err) {
    console.error(`failed to read ${p}: ${err.message}`);
    process.exit(1);
  }
}

const raw = estimateTokens(load(rawPath));
const agent = estimateTokens(load(agentPath));
const pct = raw ? Math.round((1 - agent / raw) * 100) : 0;

console.log(`raw spec:   ~${raw.toLocaleString()} tokens  (${rawPath})`);
console.log(`agent spec: ~${agent.toLocaleString()} tokens  (${agentPath})`);
console.log(`reduction:  ${pct}% smaller`);
