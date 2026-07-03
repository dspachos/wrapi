package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestResolveTemplate(t *testing.T) {
	ctx := compCtx{
		input: map[string]any{"name": "eng", "n": float64(3)},
		steps: map[string]stepResult{
			"team": {Response: map[string]any{"id": float64(99)}, Status: 201},
		},
	}
	// Full-match expressions preserve the raw typed value.
	if got := ctx.resolveTemplate("{{input.n}}"); got != float64(3) {
		t.Errorf("typed number resolve: %v (%T)", got, got)
	}
	if got := ctx.resolveTemplate("{{ steps.team.response.id }}"); got != float64(99) {
		t.Errorf("step response resolve: %v", got)
	}
	if got := ctx.resolveTemplate("{{steps.team.status}}"); got != float64(201) {
		t.Errorf("step status resolve: %v", got)
	}
	// Interpolation inside a larger string is stringified.
	if got := ctx.resolveTemplate("/teams/{{steps.team.response.id}}/x"); got != "/teams/99/x" {
		t.Errorf("interpolation: %v", got)
	}
	// Nested maps resolve recursively.
	got := ctx.resolveTemplate(map[string]any{"a": "{{input.name}}", "b": float64(1)}).(map[string]any)
	if got["a"] != "eng" || got["b"] != float64(1) {
		t.Errorf("map resolve: %v", got)
	}
	// Unknown reference => nil (full match) so callers can detect a miss.
	if got := ctx.resolveTemplate("{{input.missing}}"); got != nil {
		t.Errorf("missing should resolve to nil, got %v", got)
	}
}

// newUpstream returns an httptest server recording "METHOD path" calls, with a
// per-route handler map.
func newUpstream(routes map[string]func() (int, string)) (*httptest.Server, *[]string) {
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		calls = append(calls, key)
		if h, ok := routes[key]; ok {
			status, body := h()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, body)
			return
		}
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "{}")
	}))
	return srv, &calls
}

func newTestGateway(t *testing.T, srv *httptest.Server) *Gateway {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	return &Gateway{upstream: u, httpClient: srv.Client()}
}

func TestCompositeHappyPath(t *testing.T) {
	srv, calls := newUpstream(map[string]func() (int, string){
		"POST /teams":                 func() (int, string) { return 201, `{"id":99}` },
		"PUT /spend/1/team/99/budget": func() (int, string) { return 200, `{"ok":true}` },
	})
	defer srv.Close()
	gw := newTestGateway(t, srv)

	comp := Composite{
		Name: "onboard", Method: "POST", Path: "/composites/onboard",
		Steps: []CompositeStep{
			{ID: "team", Method: "POST", Path: "/teams", Body: map[string]any{"name": "{{input.name}}"}},
			{ID: "budget", Method: "PUT", Path: "/spend/{{input.region}}/team/{{steps.team.response.id}}/budget",
				Body: map[string]any{"max": "{{input.budget}}"}},
		},
		Response: map[string]any{"team_id": "{{steps.team.response.id}}", "ok": true},
	}

	req := httptest.NewRequest("POST", "/composites/onboard", strings.NewReader(`{"name":"eng","region":1,"budget":500}`))
	rec := httptest.NewRecorder()
	gw.compositeHandler(comp).ServeHTTP(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["team_id"] != float64(99) {
		t.Errorf("response did not thread step output: %v", out)
	}
	// Both steps hit upstream, in order, with the templated path.
	if len(*calls) != 2 || (*calls)[1] != "PUT /spend/1/team/99/budget" {
		t.Errorf("unexpected upstream calls: %v", *calls)
	}
}

func TestCompositeRollback(t *testing.T) {
	srv, calls := newUpstream(map[string]func() (int, string){
		"POST /teams":                 func() (int, string) { return 201, `{"id":99}` },
		"PUT /spend/1/team/99/budget": func() (int, string) { return 500, `{"error":"boom"}` },
		"DELETE /teams/99":            func() (int, string) { return 204, `` },
	})
	defer srv.Close()
	gw := newTestGateway(t, srv)

	comp := Composite{
		Name: "onboard", Method: "POST", Path: "/composites/onboard",
		Steps: []CompositeStep{
			{ID: "team", Method: "POST", Path: "/teams", Body: map[string]any{"name": "{{input.name}}"},
				Rollback: &RollbackAction{Method: "DELETE", Path: "/teams/{{steps.team.response.id}}"}},
			{ID: "budget", Method: "PUT", Path: "/spend/{{input.region}}/team/{{steps.team.response.id}}/budget"},
		},
	}

	req := httptest.NewRequest("POST", "/composites/onboard", strings.NewReader(`{"name":"eng","region":1}`))
	rec := httptest.NewRecorder()
	gw.compositeHandler(comp).ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 on step failure, got %d", rec.Code)
	}
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out["error"] != "composite_step_failed" {
		t.Errorf("expected composite_step_failed envelope: %v", out)
	}
	// The failed step triggers the completed step's rollback.
	last := (*calls)[len(*calls)-1]
	if last != "DELETE /teams/99" {
		t.Errorf("expected rollback DELETE /teams/99, calls = %v", *calls)
	}
}

func TestInjectComposites(t *testing.T) {
	spec := []byte(`{"paths":{"/existing":{"get":{}}}}`)
	out := injectComposites(spec, []Composite{
		{Name: "onboard_team", Method: "POST", Path: "/composites/onboard", Summary: "Onboard a team"},
	})
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	paths := doc["paths"].(map[string]any)
	if _, ok := paths["/existing"]; !ok {
		t.Error("existing paths should be preserved")
	}
	op := paths["/composites/onboard"].(map[string]any)["post"].(map[string]any)
	if op["operationId"] != "onboard_team" || op["x-wrapi-composite"] != true {
		t.Errorf("composite not injected correctly: %v", op)
	}
}
