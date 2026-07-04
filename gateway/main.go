// Command hitl-gateway is a stateless Human-in-the-Loop (HITL) reverse-proxy
// gateway for agentic API traffic.
//
// It loads a compiler-generated policy map into memory at boot, intercepts
// high-risk requests, and enforces a stateless HITL protocol: no database,
// cache, or pending-request store is used. Interception state is carried
// entirely inside a signed, short-lived HS256 JWT that the agent echoes back
// on retry via the X-HITL-Approval header.
//
// Environment:
//
//	JWT_SECRET             HMAC signing key for HITL tokens (required)
//	LEGACY_API_URL         Upstream base URL to proxy authorized traffic to (required)
//	GATEWAY_CONFIG_PATH    Path to hitl_policy_map.json (default: config/hitl_policy_map.json)
//	LISTEN_ADDR            Address to listen on (default: :8080)
package main

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/golang-jwt/jwt/v5"
)

// ---------------------------------------------------------------------------
// Policy model
// ---------------------------------------------------------------------------

// RiskRule is a single condition evaluated against the request body.
type RiskRule struct {
	Field    string `json:"field"`
	Operator string `json:"operator"`
	Value    any    `json:"value"`
}

// Policy types. HITL is one of several; the map is a typed policy engine.
const (
	policyHITL      = "hitl"       // human approval gate (stateless JWT)
	policyAudit     = "audit"      // structured logging of matched actions
	policyPIIRedact = "pii_redact" // redact response fields on egress
	policyDryRun    = "dry_run"    // short-circuit; never call upstream
	// Recognized but not yet enforced — require a shared state backend.
	policyRateLimit   = "rate_limit"
	policyBudgetCap   = "budget_cap"
	policyIdempotency = "idempotency"
)

// implementedTypes are the policy types the runtime enforces today (all stateless).
var implementedTypes = map[string]bool{
	policyHITL: true, policyAudit: true, policyPIIRedact: true, policyDryRun: true,
}

// deferredTypes are recognized but not yet enforced (need shared state — see ROADMAP).
var deferredTypes = map[string]bool{
	policyRateLimit: true, policyBudgetCap: true, policyIdempotency: true,
}

// Policy is one typed rule loaded from the policy map. Only the fields relevant
// to its Type are used; the rest are ignored.
type Policy struct {
	Type                 string     `json:"type"` // default: "hitl"
	Path                 string     `json:"path"`
	Method               string     `json:"method"`
	RiskRules            []RiskRule `json:"risk_rules,omitempty"`
	HumanMessageTemplate string     `json:"human_message_template,omitempty"`
	Redact               []string   `json:"redact,omitempty"`           // pii_redact: response dot-paths
	AuditFields          []string   `json:"audit_fields,omitempty"`     // audit: body fields to log
	DryRunResponse       any        `json:"dry_run_response,omitempty"` // dry_run: canned simulated body

	// compiled at load time for fast matching
	re *regexp.Regexp
}

// policyType returns the normalized type, defaulting to "hitl" for backward
// compatibility with the original untyped policy map.
func (p *Policy) policyType() string {
	if strings.TrimSpace(p.Type) == "" {
		return policyHITL
	}
	return strings.ToLower(p.Type)
}

// matches reports whether this policy applies to the given method + path.
func (p *Policy) matches(method, path string) bool {
	if !strings.EqualFold(p.Method, method) {
		return false
	}
	return p.re.MatchString(path)
}

// rulesSatisfied evaluates the risk_rules against the parsed body. An empty rule
// set means "always applies". A non-empty set applies only when ALL conditions
// evaluate true (logical AND).
func (p *Policy) rulesSatisfied(body map[string]any) bool {
	if len(p.RiskRules) == 0 {
		return true
	}
	for _, r := range p.RiskRules {
		if !evalCondition(r, body) {
			return false
		}
	}
	return true
}

// isHighRisk is the HITL-specific alias for rulesSatisfied.
func (p *Policy) isHighRisk(body map[string]any) bool { return p.rulesSatisfied(body) }

// renderMessage fills {path}, {method}, and {field} placeholders in the
// human-facing approval message.
func (p *Policy) renderMessage(method, path string, body map[string]any) string {
	out := p.HumanMessageTemplate
	out = strings.ReplaceAll(out, "{path}", path)
	out = strings.ReplaceAll(out, "{method}", method)
	out = placeholderRe.ReplaceAllStringFunc(out, func(m string) string {
		key := m[1 : len(m)-1]
		if key == "path" || key == "method" {
			return m // already handled above
		}
		if v, ok := getField(body, key); ok {
			return fmt.Sprint(v)
		}
		return m
	})
	return out
}

var placeholderRe = regexp.MustCompile(`\{([a-zA-Z0-9_.]+)\}`)

// ---------------------------------------------------------------------------
// Pattern compilation: OpenAPI path template -> anchored regexp
// ---------------------------------------------------------------------------

// patternToRegex converts a path pattern such as "/users/{id}" or "/files/*"
// into an anchored regular expression. "{name}" matches a single path
// segment; "*" matches any run of characters.
func patternToRegex(pattern string) *regexp.Regexp {
	var b strings.Builder
	b.WriteString("^")
	i := 0
	for i < len(pattern) {
		c := pattern[i]
		switch c {
		case '{':
			if j := strings.IndexByte(pattern[i:], '}'); j != -1 {
				b.WriteString(`[^/]+`)
				i += j + 1
				continue
			}
			b.WriteString(regexp.QuoteMeta(string(c)))
			i++
		case '*':
			b.WriteString(`.*`)
			i++
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
			i++
		}
	}
	b.WriteString("$")
	return regexp.MustCompile(b.String())
}

// ---------------------------------------------------------------------------
// Condition evaluation
// ---------------------------------------------------------------------------

// getField resolves a dot-separated path into a decoded JSON body.
func getField(body map[string]any, field string) (any, bool) {
	var cur any = body
	for _, part := range strings.Split(field, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[part]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// extractPathParams maps {name} template segments to their values in the actual
// request path, e.g. ("/users/{user_id}", "/users/42") -> {"user_id": "42"}.
func extractPathParams(template, path string) map[string]any {
	tSeg := strings.Split(strings.Trim(template, "/"), "/")
	pSeg := strings.Split(strings.Trim(path, "/"), "/")
	out := map[string]any{}
	if len(tSeg) != len(pSeg) {
		return out // wildcard or segment-count mismatch: skip extraction
	}
	for i, seg := range tSeg {
		if len(seg) >= 2 && seg[0] == '{' && seg[len(seg)-1] == '}' {
			out[seg[1:len(seg)-1]] = pSeg[i]
		}
	}
	return out
}

// mergeContext overlays later maps onto earlier ones (later wins). Used to build
// the rule/message evaluation context: request body < query params < path params.
func mergeContext(maps ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func toFloat(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func evalCondition(rule RiskRule, body map[string]any) bool {
	op := strings.ToUpper(rule.Operator)

	got, present := getField(body, rule.Field)
	if op == "EXISTS" {
		return present
	}
	if !present {
		return false
	}

	switch op {
	case "GT", "GTE", "LT", "LTE":
		a, ok1 := toFloat(got)
		b, ok2 := toFloat(rule.Value)
		if !ok1 || !ok2 {
			return false
		}
		switch op {
		case "GT":
			return a > b
		case "GTE":
			return a >= b
		case "LT":
			return a < b
		case "LTE":
			return a <= b
		}
	case "EQUALS", "NOT_EQUALS":
		eq := valuesEqual(got, rule.Value)
		if op == "EQUALS" {
			return eq
		}
		return !eq
	case "CONTAINS":
		return strings.Contains(fmt.Sprint(got), fmt.Sprint(rule.Value))
	}
	return false
}

// valuesEqual compares numerically when both sides are numeric, otherwise by
// string representation.
func valuesEqual(a, b any) bool {
	if fa, ok1 := toFloat(a); ok1 {
		if fb, ok2 := toFloat(b); ok2 {
			return fa == fb
		}
	}
	return fmt.Sprint(a) == fmt.Sprint(b)
}

// ---------------------------------------------------------------------------
// Gateway
// ---------------------------------------------------------------------------

type Gateway struct {
	policies         []Policy
	jwtSecret        []byte
	proxy            *httputil.ReverseProxy
	responseShapes   []ResponseShape
	normalizeErrors  bool
	projectResponses bool

	// composite orchestration
	upstream   *url.URL
	httpClient *http.Client
	composites []Composite
}

const hitlSubject = "agent_hitl_request"
const hitlTTL = 10 * time.Minute

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// matchingPolicies returns every policy whose method+path matches, in file order.
func (g *Gateway) matchingPolicies(method, path string) []*Policy {
	var out []*Policy
	for i := range g.policies {
		if g.policies[i].matches(method, path) {
			out = append(out, &g.policies[i])
		}
	}
	return out
}

func (g *Gateway) generateToken(method, path, bodyHash string) (string, error) {
	claims := jwt.MapClaims{
		"sub":       hitlSubject,
		"exp":       time.Now().Add(hitlTTL).Unix(),
		"path":      path,
		"method":    method,
		"body_hash": bodyHash,
	}
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return tok.SignedString(g.jwtSecret)
}

// validateApproval verifies signature, expiry, subject, and that the token's
// path/method/body_hash claims exactly match the current request.
func (g *Gateway) validateApproval(tokenStr, method, path, bodyHash string) error {
	tok, err := jwt.Parse(tokenStr, func(t *jwt.Token) (any, error) {
		if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
			return nil, fmt.Errorf("unexpected signing method: %v", t.Header["alg"])
		}
		return g.jwtSecret, nil
	}, jwt.WithValidMethods([]string{"HS256"}))
	if err != nil {
		return fmt.Errorf("token invalid: %w", err) // covers expired exp
	}
	if !tok.Valid {
		return errors.New("token not valid")
	}
	claims, ok := tok.Claims.(jwt.MapClaims)
	if !ok {
		return errors.New("unexpected claims type")
	}
	if sub, _ := claims["sub"].(string); sub != hitlSubject {
		return errors.New("subject mismatch")
	}
	if claims["path"] != path {
		return errors.New("path mismatch: request does not match approved action")
	}
	if claims["method"] != method {
		return errors.New("method mismatch: request does not match approved action")
	}
	if claims["body_hash"] != bodyHash {
		return errors.New("body_hash mismatch: payload was modified after approval")
	}
	return nil
}

type approvalRequiredResponse struct {
	Error            string `json:"error"`
	AgentInstruction string `json:"agent_instruction"`
	MessageForHuman  string `json:"message_for_human"`
	HITLToken        string `json:"hitl_token"`
}

func writeApprovalRequired(w http.ResponseWriter, message, token string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_ = json.NewEncoder(w).Encode(approvalRequiredResponse{
		Error: "human_approval_required",
		AgentInstruction: "STOP — do not proceed on your own. Show `message_for_human` to a human " +
			"operator and wait for their decision. Only if the human approves, retry this EXACT " +
			"request (same method, path, and body) with the header `X-HITL-Approval: <hitl_token>`. " +
			"If the human declines, abort and do not retry.",
		MessageForHuman: message,
		HITLToken:       token,
	})
}

// audit emits a structured log line for a matched action.
func (g *Gateway) audit(p *Policy, r *http.Request, body map[string]any) {
	fields := map[string]any{}
	for _, f := range p.AuditFields {
		if v, ok := getField(body, f); ok {
			fields[f] = v
		}
	}
	entry, _ := json.Marshal(map[string]any{
		"audit":  true,
		"method": r.Method,
		"path":   r.URL.Path,
		"remote": r.RemoteAddr,
		"fields": fields,
	})
	log.Printf("%s", entry)
}

// writeDryRun short-circuits a request: it is never sent upstream.
func writeDryRun(w http.ResponseWriter, p *Policy, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	resp := map[string]any{
		"dry_run": true,
		"method":  r.Method,
		"path":    r.URL.Path,
		"message": "Dry run: the request was NOT sent upstream.",
	}
	if p.DryRunResponse != nil {
		resp["simulated_response"] = p.DryRunResponse
	}
	_ = json.NewEncoder(w).Encode(resp)
}

// Middleware runs the ingress policy chain (audit, dry_run, hitl) for a request.
// Egress policies (pii_redact) run later in modifyResponse. It stays stateless:
// interception state lives only in the signed HITL token.
func (g *Gateway) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Buffer the body so it can be hashed and still forwarded upstream.
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "failed to read request body", http.StatusBadRequest)
			return
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(bodyBytes))

		matches := g.matchingPolicies(r.Method, r.URL.Path)
		if len(matches) == 0 {
			next.ServeHTTP(w, r)
			return
		}

		// Build the base rule/message context from the request body + query
		// params. Path params are merged in per-policy (each template differs).
		base := map[string]any{}
		if len(bytes.TrimSpace(bodyBytes)) > 0 {
			var parsed map[string]any
			if json.Unmarshal(bodyBytes, &parsed) == nil {
				for k, v := range parsed {
					base[k] = v
				}
			}
		}
		for k, vs := range r.URL.Query() {
			if len(vs) > 0 {
				base[k] = vs[0]
			}
		}

		// Dispatch ingress policies by type. Precedence: audit (record) ->
		// dry_run (short-circuit) -> hitl (approval gate).
		var dryRun, hitl *Policy
		var hitlCtx map[string]any
		for _, p := range matches {
			pctx := mergeContext(base, extractPathParams(p.Path, r.URL.Path))
			if !p.rulesSatisfied(pctx) {
				continue
			}
			switch p.policyType() {
			case policyAudit:
				g.audit(p, r, pctx)
			case policyDryRun:
				if dryRun == nil {
					dryRun = p
				}
			case policyHITL:
				if hitl == nil {
					hitl = p
					hitlCtx = pctx
				}
			}
		}

		if dryRun != nil {
			writeDryRun(w, dryRun, r)
			return
		}

		if hitl != nil {
			// Stateless HITL protocol.
			bodyHash := sha256Hex(bodyBytes)
			if approval := r.Header.Get("X-HITL-Approval"); approval != "" {
				if err := g.validateApproval(approval, r.Method, r.URL.Path, bodyHash); err == nil {
					next.ServeHTTP(w, r) // approved + integrity-checked
					return
				} else {
					log.Printf("hitl: rejected approval token for %s %s: %v", r.Method, r.URL.Path, err)
				}
			}
			token, err := g.generateToken(r.Method, r.URL.Path, bodyHash)
			if err != nil {
				http.Error(w, "failed to mint approval token", http.StatusInternalServerError)
				return
			}
			writeApprovalRequired(w, hitl.renderMessage(r.Method, r.URL.Path, hitlCtx), token)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// Agent-friendly error normalization
// ---------------------------------------------------------------------------
//
// Upstream APIs return errors in wildly different shapes (cryptic 4xx JSON, HTML
// 5xx stack traces, plain text). Agents waste tokens and retries decoding them.
// The gateway rewrites every upstream error (and connection failure) into one
// consistent envelope with a normalized code, a human/agent-readable message,
// and an actionable "fix" hint.

// agentError is the normalized error envelope returned to agents.
type agentError struct {
	Error   string `json:"error"`             // stable slug, e.g. "not_found"
	Status  int    `json:"status"`            // HTTP status code
	Message string `json:"message"`           // concise description
	Fix     string `json:"fix,omitempty"`     // actionable hint for the agent
	Details any    `json:"details,omitempty"` // upstream detail (4xx only)
}

// classifyStatus maps an HTTP status to a stable slug and an actionable fix hint.
func classifyStatus(status int) (slug, fix string) {
	switch status {
	case http.StatusBadRequest:
		return "bad_request", "The request was malformed. Check the request body, query params, and path values."
	case http.StatusUnauthorized:
		return "unauthorized", "Authentication failed upstream. Verify the credentials the gateway forwards."
	case http.StatusForbidden:
		return "forbidden", "The upstream denied this action. The caller may lack permission for it."
	case http.StatusNotFound:
		return "not_found", "The resource was not found. Verify the path and any {id} values in it."
	case http.StatusMethodNotAllowed:
		return "method_not_allowed", "This HTTP method is not allowed on this path. Check the operation."
	case http.StatusConflict:
		return "conflict", "The request conflicts with current state (e.g. a duplicate). Fetch current state and reconcile."
	case http.StatusUnprocessableEntity:
		return "unprocessable_entity", "Validation failed. Inspect `details` and correct the offending fields."
	case http.StatusTooManyRequests:
		return "rate_limited", "Rate limited upstream. Wait and retry with backoff (see the Retry-After header)."
	default:
		if status >= 500 {
			return "upstream_error", "The upstream service errored. This may be transient; retry with backoff."
		}
		return "client_error", "The request was rejected. Inspect `details` and adjust the call."
	}
}

// messageFromJSON pulls a human-readable message out of a decoded JSON error body.
func messageFromJSON(v any) string {
	m, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range []string{"message", "detail", "error_description", "title", "error"} {
		if s, ok := m[key].(string); ok && strings.TrimSpace(s) != "" {
			return s
		}
	}
	return ""
}

// readAndDecode reads an upstream body (transparently gunzipping) up to a cap.
func readAndDecode(resp *http.Response) []byte {
	defer resp.Body.Close()
	var reader io.Reader = resp.Body
	if strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		if gz, err := gzip.NewReader(resp.Body); err == nil {
			defer gz.Close()
			reader = gz
		}
	}
	raw, _ := io.ReadAll(io.LimitReader(reader, 1<<20)) // cap at 1 MiB
	return raw
}

// buildEnvelope turns a status + raw upstream body into a normalized envelope.
// 5xx bodies are NOT echoed (they leak stack traces / internals); 4xx details
// are preserved because they are usually actionable (validation errors, etc.).
func buildEnvelope(status int, raw []byte) agentError {
	slug, fix := classifyStatus(status)
	env := agentError{Error: slug, Status: status, Fix: fix, Message: http.StatusText(status)}

	if status >= 500 || len(bytes.TrimSpace(raw)) == 0 {
		if status >= 500 {
			env.Message = "The upstream service returned an error."
		}
		return env
	}

	var parsed any
	if err := json.Unmarshal(raw, &parsed); err == nil {
		if msg := messageFromJSON(parsed); msg != "" {
			env.Message = msg
		}
		env.Details = parsed
		return env
	}

	// Non-JSON 4xx body: surface a truncated snippet as the message.
	s := strings.TrimSpace(string(raw))
	if len(s) > 500 {
		s = s[:500] + "…"
	}
	if s != "" {
		env.Message = s
	}
	return env
}

// normalizeErrorResponse is the ReverseProxy ModifyResponse hook: it rewrites
// any upstream 4xx/5xx into the agentError envelope. 2xx/3xx pass through.
func normalizeErrorResponse(resp *http.Response) error {
	if resp.StatusCode < 400 {
		return nil
	}
	env := buildEnvelope(resp.StatusCode, readAndDecode(resp))
	body, err := json.Marshal(env)
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
	resp.Header.Del("Content-Encoding") // body is now plain JSON
	return nil
}

// writeAgentError emits an agentError envelope directly (used when there is no
// upstream response at all, e.g. a connection failure).
func writeAgentError(w http.ResponseWriter, status int, slug, message, fix string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(agentError{Error: slug, Status: status, Message: message, Fix: fix})
}

// ---------------------------------------------------------------------------
// Response projection (cost control)
// ---------------------------------------------------------------------------
//
// Legacy responses are fat: an agent pays input tokens for every field it reads
// back, on every loop. A ResponseShape (compiled from the spec) trims a 2xx JSON
// body to only the fields the agent needs, and caps list length. Operations with
// no shape pass through untouched — projection never removes what it isn't sure
// about.

// ResponseShape is a per-operation egress projection.
type ResponseShape struct {
	Path     string   `json:"path"`
	Method   string   `json:"method"`
	Include  []string `json:"include"`             // dot-paths to keep (per record)
	MaxItems int      `json:"max_items"`           // cap on list length (0 = no cap)
	ListPath string   `json:"list_path,omitempty"` // dot-path to the array inside a wrapper (e.g. "data")

	re *regexp.Regexp
}

// findShape returns the first response shape matching method+path, or nil.
func (g *Gateway) findShape(method, path string) *ResponseShape {
	for i := range g.responseShapes {
		s := &g.responseShapes[i]
		if strings.EqualFold(s.Method, method) && s.re.MatchString(path) {
			return s
		}
	}
	return nil
}

// projectValue applies a shape to a decoded JSON value.
func projectValue(v any, include []string, maxItems int) any {
	switch t := v.(type) {
	case []any:
		if maxItems > 0 && len(t) > maxItems {
			t = t[:maxItems]
		}
		if len(include) > 0 {
			out := make([]any, len(t))
			for i, e := range t {
				out[i] = pickPaths(e, include)
			}
			return out
		}
		return t
	case map[string]any:
		if len(include) > 0 {
			return pickPaths(t, include)
		}
		return t
	default:
		return v
	}
}

// commonListKeys are the wrapper fields wrapi will auto-detect as the record
// array when a shape has no explicit list_path.
var commonListKeys = []string{"data", "items", "results", "records"}

// applyShape projects a decoded response per a shape. It handles three shapes:
//   - explicit list_path -> project the array at that dot-path, leave the wrapper
//   - a bare top-level array or object -> project directly
//   - an object wrapping a list under a common key (data/items/...) -> project it
func applyShape(v any, shape *ResponseShape) any {
	if shape.ListPath != "" {
		return projectAtPath(v, strings.Split(shape.ListPath, "."), shape)
	}
	if m, ok := v.(map[string]any); ok {
		for _, key := range commonListKeys {
			if arr, ok := m[key].([]any); ok {
				m[key] = projectValue(arr, shape.Include, shape.MaxItems)
				return m
			}
		}
	}
	return projectValue(v, shape.Include, shape.MaxItems)
}

// projectAtPath projects the array located at the given dot-path inside an
// object, leaving the surrounding wrapper (e.g. paging metadata) intact.
func projectAtPath(v any, parts []string, shape *ResponseShape) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	cur := m
	for i := 0; i < len(parts)-1; i++ {
		next, ok := cur[parts[i]].(map[string]any)
		if !ok {
			return v // path not found; leave unchanged
		}
		cur = next
	}
	last := parts[len(parts)-1]
	if arr, ok := cur[last].([]any); ok {
		cur[last] = projectValue(arr, shape.Include, shape.MaxItems)
	}
	return m
}

// pickPaths returns a new object containing only the given dot-paths.
func pickPaths(v any, paths []string) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := map[string]any{}
	for _, p := range paths {
		if val, ok := getField(m, p); ok {
			setPath(out, strings.Split(p, "."), val)
		}
	}
	return out
}

// setPath writes val into dst at the nested key path, creating maps as needed.
func setPath(dst map[string]any, parts []string, val any) {
	for i := 0; i < len(parts)-1; i++ {
		next, ok := dst[parts[i]].(map[string]any)
		if !ok {
			next = map[string]any{}
			dst[parts[i]] = next
		}
		dst = next
	}
	dst[parts[len(parts)-1]] = val
}

// resetBody restores a (possibly already-consumed) body onto the response.
func resetBody(resp *http.Response, body []byte) {
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	resp.Header.Del("Content-Encoding") // body is decoded/plain now
	resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
}

// redactValue replaces the given dot-paths with "[REDACTED]" in a decoded JSON
// value. For arrays it applies per element (like projection).
func redactValue(v any, paths []string) any {
	switch t := v.(type) {
	case []any:
		for i := range t {
			t[i] = redactValue(t[i], paths)
		}
		return t
	case map[string]any:
		for _, p := range paths {
			if _, ok := getField(t, p); ok {
				setPath(t, strings.Split(p, "."), "[REDACTED]")
			}
		}
		return t
	default:
		return v
	}
}

// transformSuccess applies egress transforms to a 2xx JSON body: response
// projection (Phase 2) then PII redaction (pii_redact policies). If neither
// applies, the body is left untouched.
func (g *Gateway) transformSuccess(resp *http.Response) error {
	if resp.Request == nil {
		return nil
	}
	method, path := resp.Request.Method, resp.Request.URL.Path

	var shape *ResponseShape
	if g.projectResponses {
		shape = g.findShape(method, path)
	}
	var redactors []*Policy
	for _, p := range g.matchingPolicies(method, path) {
		if p.policyType() == policyPIIRedact && len(p.Redact) > 0 {
			redactors = append(redactors, p)
		}
	}
	if shape == nil && len(redactors) == 0 {
		return nil // nothing to do
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "json") {
		return nil
	}

	raw := readAndDecode(resp) // consumes + closes the original body
	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		resetBody(resp, raw) // not JSON we can transform; hand it back unchanged
		return nil
	}

	if shape != nil {
		parsed = applyShape(parsed, shape)
	}
	for _, p := range redactors {
		parsed = redactValue(parsed, p.Redact)
	}

	body, err := json.Marshal(parsed)
	if err != nil {
		resetBody(resp, raw)
		return nil
	}
	if len(body) < len(raw) {
		saved := 100 - (len(body) * 100 / max(1, len(raw)))
		log.Printf("egress %s %s: %d -> %d bytes (-%d%%)", method, path, len(raw), len(body), saved)
	}
	resp.Header.Set("Content-Type", "application/json")
	resetBody(resp, body)
	return nil
}

// modifyResponse is the single ReverseProxy egress hook: normalize errors on
// 4xx/5xx, transform (project + redact) successful JSON on 2xx.
func (g *Gateway) modifyResponse(resp *http.Response) error {
	if resp.StatusCode >= 400 {
		if g.normalizeErrors {
			return normalizeErrorResponse(resp)
		}
		return nil
	}
	return g.transformSuccess(resp)
}

// ---------------------------------------------------------------------------
// Boot
// ---------------------------------------------------------------------------

func loadResponseShapes(path string) ([]ResponseShape, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var shapes []ResponseShape
	if err := json.Unmarshal(data, &shapes); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	for i := range shapes {
		shapes[i].re = patternToRegex(shapes[i].Path)
	}
	return shapes, nil
}

func loadPolicies(path string) ([]Policy, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var policies []Policy
	if err := json.Unmarshal(data, &policies); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	for i := range policies {
		policies[i].re = patternToRegex(policies[i].Path)
		t := policies[i].policyType()
		switch {
		case implementedTypes[t]:
			// ok
		case deferredTypes[t]:
			log.Printf("warning: policy %s %s type %q is recognized but not yet enforced (needs a state backend; see ROADMAP)",
				policies[i].Method, policies[i].Path, t)
		default:
			log.Printf("warning: policy %s %s has unknown type %q; it will be ignored",
				policies[i].Method, policies[i].Path, t)
		}
	}
	return policies, nil
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("required environment variable %s is not set", key)
	}
	return v
}

// envDisabled reports whether an on-by-default feature is switched off via env.
func envDisabled(key string) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "off", "false", "0", "no":
		return true
	}
	return false
}

func main() {
	jwtSecret := mustEnv("JWT_SECRET")
	legacyURL := mustEnv("LEGACY_API_URL")

	configPath := os.Getenv("GATEWAY_CONFIG_PATH")
	if configPath == "" {
		configPath = "config/hitl_policy_map.json"
	}
	agentSpecPath := os.Getenv("AGENT_SPEC_PATH")
	if agentSpecPath == "" {
		agentSpecPath = "config/agent_openapi.json"
	}
	listenAddr := os.Getenv("LISTEN_ADDR")
	if listenAddr == "" {
		listenAddr = ":8080"
	}

	target, err := url.Parse(legacyURL)
	if err != nil {
		log.Fatalf("invalid LEGACY_API_URL %q: %v", legacyURL, err)
	}

	policies, err := loadPolicies(configPath)
	if err != nil {
		log.Fatalf("failed to load policy map: %v", err)
	}

	// The stripped, prompt-optimized agent spec is served (not enforced), so
	// agents can discover the exact API surface this gateway fronts. Optional:
	// a missing file just disables the discovery endpoint.
	agentSpec, err := os.ReadFile(agentSpecPath)
	if err != nil {
		log.Printf("warning: agent spec not loaded (%v); discovery endpoint disabled", err)
		agentSpec = nil
	}

	// Egress transforms are on by default; disable with ERROR_NORMALIZATION=off
	// / RESPONSE_PROJECTION=off (or false/0/no).
	normalizeErrors := !envDisabled("ERROR_NORMALIZATION")
	projectResponses := !envDisabled("RESPONSE_PROJECTION")

	// Response shapes are optional; a missing file just disables projection.
	shapesPath := os.Getenv("RESPONSE_SHAPES_PATH")
	if shapesPath == "" {
		shapesPath = "config/response_shapes.json"
	}
	shapes, err := loadResponseShapes(shapesPath)
	if err != nil {
		log.Printf("warning: response shapes not loaded (%v); projection disabled", err)
		shapes = nil
	}

	// Composite operations are optional; a missing file just disables them.
	compositesPath := os.Getenv("COMPOSITES_PATH")
	if compositesPath == "" {
		compositesPath = "config/composites.json"
	}
	composites, err := loadComposites(compositesPath)
	if err != nil {
		log.Printf("warning: composites not loaded (%v); none registered", err)
		composites = nil
	}
	// Make composite operations discoverable in the served agent spec.
	if len(composites) > 0 && agentSpec != nil {
		agentSpec = injectComposites(agentSpec, composites)
	}

	proxy := httputil.NewSingleHostReverseProxy(target)
	// Preserve the default director but rewrite Host to the upstream so
	// virtual-hosted backends route correctly.
	baseDirector := proxy.Director
	proxy.Director = func(req *http.Request) {
		baseDirector(req)
		req.Host = target.Host
	}
	// Connection-level failures (upstream unreachable) become a structured envelope.
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("proxy error for %s %s: %v", r.Method, r.URL.Path, err)
		writeAgentError(w, http.StatusBadGateway, "upstream_unreachable",
			"The upstream service could not be reached.",
			"The gateway could not connect upstream. Retry later; if it persists the upstream may be down.")
	}

	gw := &Gateway{
		policies:         policies,
		jwtSecret:        []byte(jwtSecret),
		proxy:            proxy,
		responseShapes:   shapes,
		normalizeErrors:  normalizeErrors,
		projectResponses: projectResponses,
		upstream:         target,
		httpClient:       &http.Client{Timeout: 30 * time.Second},
		composites:       composites,
	}
	// Single egress hook: normalize errors + project successful responses.
	proxy.ModifyResponse = gw.modifyResponse

	r := chi.NewRouter()

	// Liveness probe (never intercepted or proxied).
	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok","policies":` + strconv.Itoa(len(policies)) + `}`))
	})

	// Agent-facing spec discovery: the stripped, function-calling-optimized
	// OpenAPI spec for exactly the surface this gateway fronts. Served locally
	// (not proxied) so it never depends on the upstream being reachable.
	//
	// Served at the conventional /openapi.json (max tooling compatibility) and
	// at an explicit /.well-known/ alias. This intentionally SHADOWS the
	// origin's own /openapi.json: an agent talking to the gateway should
	// discover the gateway-scoped spec, not the upstream's full surface.
	serveAgentSpec := func(w http.ResponseWriter, _ *http.Request) {
		if agentSpec == nil {
			http.Error(w, "agent spec not available", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(agentSpec)
	}
	r.Get("/openapi.json", serveAgentSpec)
	r.Get("/.well-known/agent-openapi.json", serveAgentSpec)

	// Everything runs through the policy middleware. Composite operations are
	// registered as explicit routes (so policies still apply to them); all other
	// paths fall through to the reverse proxy.
	r.Group(func(pr chi.Router) {
		pr.Use(gw.Middleware)
		for _, c := range composites {
			pr.MethodFunc(strings.ToUpper(c.Method), c.Path, gw.compositeHandler(c))
			log.Printf("  composite: %s %s (%d step(s)) -> %s", strings.ToUpper(c.Method), c.Path, len(c.Steps), c.Name)
		}
		pr.Handle("/*", proxy)
	})

	log.Printf("hitl-gateway listening on %s", listenAddr)
	log.Printf("  upstream (LEGACY_API_URL): %s", target.String())
	log.Printf("  loaded %d HITL polic%s from %s", len(policies), plural(len(policies)), configPath)
	log.Printf("  agent-friendly error normalization: %v", normalizeErrors)
	log.Printf("  response projection: %v (%d shape(s))", projectResponses, len(shapes))
	log.Printf("  composite operations: %d", len(composites))

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Fatal(srv.ListenAndServe())
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
