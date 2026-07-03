package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPatternToRegex(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"/users/{id}", "/users/123", true},
		{"/users/{id}", "/users/123/roles", false},
		{"/users/{id}", "/users/", false},
		{"/users", "/users", true},
		{"/files/*", "/files/a/b/c", true},
		{"/files/*", "/other", false},
		{"/a/{x}/b/{y}", "/a/1/b/2", true},
		{"/a/{x}/b/{y}", "/a/1/b", false},
	}
	for _, c := range cases {
		got := patternToRegex(c.pattern).MatchString(c.path)
		if got != c.want {
			t.Errorf("patternToRegex(%q).Match(%q) = %v, want %v", c.pattern, c.path, got, c.want)
		}
	}
}

func TestGetField(t *testing.T) {
	body := map[string]any{
		"amount": 500.0,
		"customer": map[string]any{
			"tier": "gold",
		},
	}
	if v, ok := getField(body, "amount"); !ok || v != 500.0 {
		t.Errorf("getField amount = %v, %v", v, ok)
	}
	if v, ok := getField(body, "customer.tier"); !ok || v != "gold" {
		t.Errorf("getField customer.tier = %v, %v", v, ok)
	}
	if _, ok := getField(body, "customer.missing"); ok {
		t.Error("getField customer.missing should be absent")
	}
	if _, ok := getField(body, "nope"); ok {
		t.Error("getField nope should be absent")
	}
}

func TestEvalCondition(t *testing.T) {
	body := map[string]any{
		"amount":   500.0,
		"currency": "USD",
		"is_admin": true,
	}
	cases := []struct {
		name string
		rule RiskRule
		want bool
	}{
		{"GT true", RiskRule{"amount", "GT", 100.0}, true},
		{"GT false", RiskRule{"amount", "GT", 1000.0}, false},
		{"GTE equal", RiskRule{"amount", "GTE", 500.0}, true},
		{"LT true", RiskRule{"amount", "LT", 1000.0}, true},
		{"EQUALS string", RiskRule{"currency", "EQUALS", "USD"}, true},
		{"NOT_EQUALS", RiskRule{"currency", "NOT_EQUALS", "EUR"}, true},
		{"EQUALS bool", RiskRule{"is_admin", "EQUALS", true}, true},
		{"EXISTS present", RiskRule{"amount", "EXISTS", nil}, true},
		{"EXISTS absent", RiskRule{"missing", "EXISTS", nil}, false},
		{"CONTAINS", RiskRule{"currency", "CONTAINS", "US"}, true},
		{"missing field non-exists", RiskRule{"missing", "GT", 1.0}, false},
	}
	for _, c := range cases {
		if got := evalCondition(c.rule, body); got != c.want {
			t.Errorf("%s: evalCondition = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestApprovalRoundTrip(t *testing.T) {
	gw := &Gateway{jwtSecret: []byte("test-secret")}
	hash := sha256Hex([]byte(`{"amount":500}`))

	tok, err := gw.generateToken("POST", "/payments", hash)
	if err != nil {
		t.Fatalf("generateToken: %v", err)
	}

	// Exact match validates.
	if err := gw.validateApproval(tok, "POST", "/payments", hash); err != nil {
		t.Errorf("valid token rejected: %v", err)
	}

	// Tamper detection: body, path, method each must break validation.
	if err := gw.validateApproval(tok, "POST", "/payments", sha256Hex([]byte(`{"amount":999}`))); err == nil {
		t.Error("tampered body_hash should be rejected")
	}
	if err := gw.validateApproval(tok, "POST", "/other", hash); err == nil {
		t.Error("mismatched path should be rejected")
	}
	if err := gw.validateApproval(tok, "DELETE", "/payments", hash); err == nil {
		t.Error("mismatched method should be rejected")
	}

	// Wrong signing key must be rejected.
	other := &Gateway{jwtSecret: []byte("different-secret")}
	if err := other.validateApproval(tok, "POST", "/payments", hash); err == nil {
		t.Error("token signed with a different key should be rejected")
	}
}

func TestBuildEnvelope(t *testing.T) {
	// 4xx JSON: message extracted, details preserved.
	env := buildEnvelope(422, []byte(`{"detail":"amount must be positive","field":"amount"}`))
	if env.Error != "unprocessable_entity" || env.Status != 422 {
		t.Errorf("unexpected slug/status: %+v", env)
	}
	if env.Message != "amount must be positive" {
		t.Errorf("message not extracted: %q", env.Message)
	}
	if env.Details == nil {
		t.Error("4xx details should be preserved")
	}
	if env.Fix == "" {
		t.Error("expected a fix hint")
	}

	// 5xx: body NOT echoed (no leaking internals).
	env = buildEnvelope(500, []byte("Traceback (most recent call last): secret stack trace"))
	if env.Error != "upstream_error" {
		t.Errorf("expected upstream_error, got %q", env.Error)
	}
	if env.Details != nil {
		t.Error("5xx details must not be echoed")
	}

	// Non-JSON 4xx: snippet surfaced as message.
	env = buildEnvelope(400, []byte("bad input"))
	if env.Message != "bad input" {
		t.Errorf("expected snippet message, got %q", env.Message)
	}

	// Empty body: falls back to status text.
	env = buildEnvelope(404, nil)
	if env.Error != "not_found" || env.Message == "" {
		t.Errorf("unexpected empty-body envelope: %+v", env)
	}
}

func TestNormalizeErrorResponse(t *testing.T) {
	// 2xx passes through untouched.
	ok := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader([]byte("hello")))}
	if err := normalizeErrorResponse(ok); err != nil {
		t.Fatalf("normalizeErrorResponse(200): %v", err)
	}
	if b, _ := io.ReadAll(ok.Body); string(b) != "hello" {
		t.Errorf("2xx body was modified: %q", b)
	}

	// 404 gets rewritten into the envelope.
	resp := &http.Response{
		StatusCode: 404,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"message":"no such user"}`))),
	}
	if err := normalizeErrorResponse(resp); err != nil {
		t.Fatalf("normalizeErrorResponse(404): %v", err)
	}
	var env agentError
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("rewritten body is not valid envelope JSON: %v", err)
	}
	if env.Error != "not_found" || env.Message != "no such user" {
		t.Errorf("unexpected envelope: %+v", env)
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Error("Content-Type should be application/json")
	}
}

func TestPickPaths(t *testing.T) {
	rec := map[string]any{
		"id":     1.0,
		"email":  "a@b.com",
		"secret": "hunter2",
		"team":   map[string]any{"id": 7.0, "name": "eng", "budget": 999.0},
	}
	got := pickPaths(rec, []string{"id", "email", "team.id"}).(map[string]any)
	if got["id"] != 1.0 || got["email"] != "a@b.com" {
		t.Errorf("missing top-level fields: %+v", got)
	}
	if _, ok := got["secret"]; ok {
		t.Error("secret should have been dropped")
	}
	team, ok := got["team"].(map[string]any)
	if !ok || team["id"] != 7.0 {
		t.Errorf("nested team.id not projected: %+v", got["team"])
	}
	if _, ok := team["budget"]; ok {
		t.Error("team.budget should have been dropped")
	}
}

func TestProjectValue(t *testing.T) {
	// List: cap + per-element projection.
	list := []any{
		map[string]any{"id": 1.0, "x": "a", "y": "drop"},
		map[string]any{"id": 2.0, "x": "b", "y": "drop"},
		map[string]any{"id": 3.0, "x": "c", "y": "drop"},
	}
	out := projectValue(list, []string{"id", "x"}, 2).([]any)
	if len(out) != 2 {
		t.Fatalf("max_items not applied: len=%d", len(out))
	}
	first := out[0].(map[string]any)
	if first["id"] != 1.0 || first["x"] != "a" {
		t.Errorf("element not projected: %+v", first)
	}
	if _, ok := first["y"]; ok {
		t.Error("y should have been dropped from list element")
	}

	// No include, no cap: value returned unchanged.
	obj := map[string]any{"a": 1.0}
	if projectValue(obj, nil, 0).(map[string]any)["a"] != 1.0 {
		t.Error("passthrough object mangled")
	}
}

func TestProjectResponsePassthroughWhenNoShape(t *testing.T) {
	gw := &Gateway{projectResponses: true} // no shapes loaded
	body := []byte(`{"a":1,"b":2}`)
	req, _ := http.NewRequest("GET", "http://x/anything", nil)
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(body)),
		Request:    req,
	}
	if err := gw.modifyResponse(resp); err != nil {
		t.Fatal(err)
	}
	if b, _ := io.ReadAll(resp.Body); string(b) != string(body) {
		t.Errorf("body changed despite no shape: %q", b)
	}
}

func TestProjectResponseWithShape(t *testing.T) {
	gw := &Gateway{
		projectResponses: true,
		responseShapes: []ResponseShape{
			{Path: "/users/{id}", Method: "GET", Include: []string{"id", "email"}, re: patternToRegex("/users/{id}")},
		},
	}
	req, _ := http.NewRequest("GET", "http://x/users/42", nil)
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"id":42,"email":"a@b.com","secret":"x","bio":"long..."}`))),
		Request:    req,
	}
	if err := gw.modifyResponse(resp); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["email"] != "a@b.com" {
		t.Errorf("projection wrong: %+v", got)
	}
	if _, ok := got["secret"]; ok {
		t.Error("secret should have been projected out")
	}
}

func TestPolicyTypeDefault(t *testing.T) {
	if (&Policy{}).policyType() != "hitl" {
		t.Error("empty type should default to hitl")
	}
	if (&Policy{Type: "AUDIT"}).policyType() != "audit" {
		t.Error("type should be lowercased")
	}
}

func TestMatchingPolicies(t *testing.T) {
	gw := &Gateway{policies: []Policy{
		{Type: "audit", Method: "POST", Path: "/pay", re: patternToRegex("/pay")},
		{Type: "hitl", Method: "POST", Path: "/pay", re: patternToRegex("/pay")},
		{Type: "hitl", Method: "GET", Path: "/pay", re: patternToRegex("/pay")},
	}}
	if got := gw.matchingPolicies("POST", "/pay"); len(got) != 2 {
		t.Errorf("expected 2 matches, got %d", len(got))
	}
}

func TestRedactValue(t *testing.T) {
	rec := map[string]any{
		"id":   1.0,
		"ssn":  "123-45-6789",
		"card": map[string]any{"num": "4111", "exp": "12/30"},
	}
	redactValue(rec, []string{"ssn", "card.num"})
	if rec["ssn"] != "[REDACTED]" {
		t.Errorf("ssn not redacted: %v", rec["ssn"])
	}
	card := rec["card"].(map[string]any)
	if card["num"] != "[REDACTED]" {
		t.Errorf("card.num not redacted: %v", card["num"])
	}
	if card["exp"] != "12/30" || rec["id"] != 1.0 {
		t.Error("non-redacted fields were altered")
	}
}

func TestMiddlewareDryRun(t *testing.T) {
	gw := &Gateway{policies: []Policy{
		{Type: "dry_run", Method: "POST", Path: "/pay", re: patternToRegex("/pay")},
	}}
	called := false
	h := gw.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true }))

	req := httptest.NewRequest("POST", "/pay", strings.NewReader(`{"amount":5}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if called {
		t.Error("dry_run must NOT forward upstream")
	}
	if rec.Code != http.StatusOK {
		t.Errorf("dry_run status = %d, want 200", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["dry_run"] != true {
		t.Errorf("expected dry_run body, got %v", body)
	}
}

func TestMiddlewareHITLStillGates(t *testing.T) {
	// Untyped policy => defaults to hitl.
	gw := &Gateway{jwtSecret: []byte("s"), policies: []Policy{
		{Method: "POST", Path: "/pay", re: patternToRegex("/pay"),
			HumanMessageTemplate: "Charge {amount}?"},
	}}
	called := false
	h := gw.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true }))

	req := httptest.NewRequest("POST", "/pay", strings.NewReader(`{"amount":5}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if called {
		t.Error("hitl must block until approved")
	}
	if rec.Code != http.StatusForbidden {
		t.Errorf("hitl status = %d, want 403", rec.Code)
	}
}

func TestMiddlewareAuditDoesNotBlock(t *testing.T) {
	gw := &Gateway{policies: []Policy{
		{Type: "audit", Method: "POST", Path: "/pay", re: patternToRegex("/pay"), AuditFields: []string{"amount"}},
	}}
	called := false
	h := gw.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { called = true; w.WriteHeader(200) }))

	req := httptest.NewRequest("POST", "/pay", strings.NewReader(`{"amount":5}`))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !called {
		t.Error("audit should log but still forward upstream")
	}
}

func TestModifyResponseRedacts(t *testing.T) {
	gw := &Gateway{policies: []Policy{
		{Type: "pii_redact", Method: "GET", Path: "/u/{id}", Redact: []string{"ssn", "card.num"}, re: patternToRegex("/u/{id}")},
	}}
	req, _ := http.NewRequest("GET", "http://x/u/5", nil)
	resp := &http.Response{
		StatusCode: 200,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(`{"id":1,"ssn":"123","card":{"num":"4111","exp":"12/30"}}`))),
		Request:    req,
	}
	if err := gw.modifyResponse(resp); err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&got)
	if got["ssn"] != "[REDACTED]" {
		t.Errorf("ssn not redacted: %v", got["ssn"])
	}
	if card := got["card"].(map[string]any); card["num"] != "[REDACTED]" || card["exp"] != "12/30" {
		t.Errorf("card redaction wrong: %v", card)
	}
}

func TestIsHighRisk(t *testing.T) {
	// Empty rules => always intercept.
	empty := &Policy{}
	if !empty.isHighRisk(map[string]any{}) {
		t.Error("empty risk_rules should always intercept")
	}

	// All rules must pass (logical AND).
	p := &Policy{RiskRules: []RiskRule{
		{"amount", "GT", 100.0},
		{"currency", "EQUALS", "USD"},
	}}
	if !p.isHighRisk(map[string]any{"amount": 500.0, "currency": "USD"}) {
		t.Error("both rules satisfied should intercept")
	}
	if p.isHighRisk(map[string]any{"amount": 500.0, "currency": "EUR"}) {
		t.Error("one rule failing should not intercept")
	}
}
