# Examples

Hand-authorable config samples for the `wrapi` gateway. Point the gateway at them
with the matching env vars (see the root `.env.example`).

| File | Env var | What it shows |
|------|---------|---------------|
| [`policy_map.json`](./policy_map.json) | `GATEWAY_CONFIG_PATH` | All four stateless policy types: `hitl`, `audit`, `pii_redact`, `dry_run`. |
| [`xquik_policy_map.json`](./xquik_policy_map.json) | `GATEWAY_CONFIG_PATH` | Xquik API policies for guarded post, direct-message, search, and account routes. |
| [`composites.json`](./composites.json) | `COMPOSITES_PATH` | A two-step composite (`onboard_team`) with templating + rollback. |

Xquik is an independent third-party service. Not affiliated with X Corp. "Twitter" and "X" are trademarks of X Corp.

> The `compiler` generates `hitl_policy_map.json`, `agent_openapi.json`, and
> `response_shapes.json` for you. Composite operations are hand-authored (for
> now). The shipped `gateway/config/` is a small generic example you can run as-is.

## Try it locally

Run a throwaway upstream, point the gateway at these examples, and exercise them.

```bash
# 1. A tiny mock upstream (returns {"id":99} for POST /teams, etc.)
cat > /tmp/upstream.mjs <<'EOF'
import http from "node:http";
http.createServer((req, res) => {
  if (req.method === "POST" && req.url === "/teams") {
    res.writeHead(201, { "content-type": "application/json" });
    return res.end('{"id":99}');
  }
  if (req.method === "PUT" && req.url.includes("/budget")) {
    res.writeHead(200, { "content-type": "application/json" });
    return res.end('{"ok":true}');
  }
  if (req.method === "GET" && req.url.startsWith("/users/")) {
    res.writeHead(200, { "content-type": "application/json" });
    return res.end('{"id":1,"name":"Ada","ssn":"123-45-6789","card":{"number":"4111"}}');
  }
  res.writeHead(200, { "content-type": "application/json" });
  res.end("{}");
}).listen(9000, () => console.log("mock upstream on :9000"));
EOF
node /tmp/upstream.mjs &

# 2. Point the gateway at the examples (needs an agent spec; an empty one is fine here)
echo '{"paths":{}}' > /tmp/agent_openapi.json
cd gateway && go build -o bin/gateway .
JWT_SECRET=dev LEGACY_API_URL=http://localhost:9000 \
  GATEWAY_CONFIG_PATH=../examples/policy_map.json \
  AGENT_SPEC_PATH=/tmp/agent_openapi.json \
  COMPOSITES_PATH=../examples/composites.json \
  ./bin/gateway &

# 3. Exercise the policies + composite
curl -s -X POST localhost:8080/emails/send -d '{}'                     # dry_run -> simulated
curl -s localhost:8080/users/1                                         # pii_redact -> ssn/card redacted
curl -s -o /dev/null -w '%{http_code}\n' -X POST localhost:8080/payments -d '{"amount":500}'  # hitl -> 403
curl -s -X POST localhost:8080/composites/onboard-team \
  -H 'content-type: application/json' \
  -d '{"name":"eng","admin_email":"a@ex.com","budget":500}'  # composite -> one call
```
