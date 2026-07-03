package main

// Composite operations collapse a multi-call workflow (create team -> add admin
// -> set budget) into a SINGLE agent-callable endpoint the gateway orchestrates.
// Fewer agent turns => fewer tokens and less error surface.
//
// Statelessness is preserved: a composite runs entirely within one request's
// lifecycle (steps + rollback happen in-memory during the call). Nothing is
// shared across requests, so replicas stay interchangeable.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
)

// Composite is one orchestrated, agent-facing operation.
type Composite struct {
	Name     string          `json:"name"`
	Method   string          `json:"method"`
	Path     string          `json:"path"`
	Summary  string          `json:"summary"`
	Steps    []CompositeStep `json:"steps"`
	Response any             `json:"response,omitempty"` // template for the final body
}

// CompositeStep is one upstream call in a composite, with an optional rollback.
type CompositeStep struct {
	ID       string          `json:"id"`
	Method   string          `json:"method"`
	Path     string          `json:"path"` // template
	Body     any             `json:"body,omitempty"`
	Rollback *RollbackAction `json:"rollback,omitempty"`
}

// RollbackAction is the compensating call run if a later step fails.
type RollbackAction struct {
	Method string `json:"method"`
	Path   string `json:"path"` // template
	Body   any    `json:"body,omitempty"`
}

func loadComposites(path string) ([]Composite, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var comps []Composite
	if err := json.Unmarshal(data, &comps); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return comps, nil
}

// ---------------------------------------------------------------------------
// Templating: {{ input.x }} and {{ steps.<id>.response.y }} / {{ steps.<id>.status }}
// ---------------------------------------------------------------------------

var tmplRe = regexp.MustCompile(`\{\{\s*([^}]+?)\s*\}\}`)

type stepResult struct {
	Response any
	Status   int
}

type compCtx struct {
	input any
	steps map[string]stepResult
}

// lookup resolves a template expression against the input + prior step results.
func (c compCtx) lookup(expr string) (any, bool) {
	parts := strings.Split(strings.TrimSpace(expr), ".")
	if len(parts) == 0 {
		return nil, false
	}
	switch parts[0] {
	case "input":
		return getAny(c.input, parts[1:])
	case "steps":
		if len(parts) < 3 {
			return nil, false
		}
		sr, ok := c.steps[parts[1]]
		if !ok {
			return nil, false
		}
		switch parts[2] {
		case "response":
			return getAny(sr.Response, parts[3:])
		case "status":
			return float64(sr.Status), true
		}
	}
	return nil, false
}

// getAny walks a decoded JSON value along a dot-path of map keys.
func getAny(v any, parts []string) (any, bool) {
	cur := v
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = m[p]; !ok {
			return nil, false
		}
	}
	return cur, true
}

// resolveTemplate recursively substitutes {{...}} in a decoded template value.
// A string that is EXACTLY one expression yields the raw (typed) value; an
// expression embedded in a larger string is stringified in place.
func (c compCtx) resolveTemplate(v any) any {
	switch t := v.(type) {
	case string:
		if m := tmplRe.FindStringSubmatch(t); m != nil && m[0] == t {
			if val, ok := c.lookup(m[1]); ok {
				return val
			}
			return nil
		}
		return tmplRe.ReplaceAllStringFunc(t, func(s string) string {
			expr := tmplRe.FindStringSubmatch(s)[1]
			if val, ok := c.lookup(expr); ok {
				return fmt.Sprint(val)
			}
			return s
		})
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, vv := range t {
			out[k] = c.resolveTemplate(vv)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, vv := range t {
			out[i] = c.resolveTemplate(vv)
		}
		return out
	default:
		return v
	}
}

// ---------------------------------------------------------------------------
// Execution
// ---------------------------------------------------------------------------

func joinURL(base *url.URL, p string) string {
	b := strings.TrimRight(base.String(), "/")
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return b + p
}

// callUpstream performs one programmatic call to LEGACY_API_URL, forwarding the
// caller's Authorization header. Returns status, parsed JSON body, and error.
func (g *Gateway) callUpstream(orig *http.Request, method, path string, body []byte) (int, any, error) {
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(orig.Context(), strings.ToUpper(method), joinURL(g.upstream, path), rdr)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if a := orig.Header.Get("Authorization"); a != "" {
		req.Header.Set("Authorization", a)
	}
	resp, err := g.httpClient.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var parsed any
	_ = json.Unmarshal(rb, &parsed)
	return resp.StatusCode, parsed, nil
}

// rollback fires compensating actions for completed steps, in reverse order.
func (g *Gateway) rollback(orig *http.Request, completed []CompositeStep, ctx compCtx) {
	for i := len(completed) - 1; i >= 0; i-- {
		rb := completed[i].Rollback
		if rb == nil {
			continue
		}
		path := fmt.Sprint(ctx.resolveTemplate(rb.Path))
		var body []byte
		if rb.Body != nil {
			body, _ = json.Marshal(ctx.resolveTemplate(rb.Body))
		}
		status, _, err := g.callUpstream(orig, rb.Method, path, body)
		if err != nil || status >= 400 {
			log.Printf("composite rollback: step %q compensation FAILED (status %d, err %v)", completed[i].ID, status, err)
		} else {
			log.Printf("composite rollback: step %q compensated", completed[i].ID)
		}
	}
}

// compositeHandler runs a composite: each step in order, rolling back completed
// steps if any step fails.
func (g *Gateway) compositeHandler(comp Composite) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var input any
		if len(bytes.TrimSpace(raw)) > 0 {
			_ = json.Unmarshal(raw, &input)
		}
		ctx := compCtx{input: input, steps: map[string]stepResult{}}
		var completed []CompositeStep

		for _, step := range comp.Steps {
			path := fmt.Sprint(ctx.resolveTemplate(step.Path))
			var reqBody []byte
			if step.Body != nil {
				reqBody, _ = json.Marshal(ctx.resolveTemplate(step.Body))
			}
			status, respBody, err := g.callUpstream(r, step.Method, path, reqBody)
			if err != nil || status >= 400 {
				g.rollback(r, completed, ctx)
				log.Printf("composite %q: step %q failed (status %d, err %v); rolled back %d step(s)",
					comp.Name, step.ID, status, err, len(completed))
				writeAgentError(w, http.StatusBadGateway, "composite_step_failed",
					fmt.Sprintf("Composite %q failed at step %q (status %d). Completed steps were rolled back.", comp.Name, step.ID, status),
					"Inspect the step inputs against the upstream requirements, then retry the composite.")
				return
			}
			ctx.steps[step.ID] = stepResult{Response: respBody, Status: status}
			completed = append(completed, step)
		}

		var out any
		if comp.Response != nil {
			out = ctx.resolveTemplate(comp.Response)
		} else {
			statuses := map[string]any{}
			for id, sr := range ctx.steps {
				statuses[id] = sr.Status
			}
			out = map[string]any{"composite": comp.Name, "steps": statuses}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(out)
	}
}

// injectComposites adds composite operations to the served agent spec so agents
// discover them alongside the real endpoints. Best-effort: returns the input
// unchanged if it can't be parsed as an OpenAPI document.
func injectComposites(specJSON []byte, comps []Composite) []byte {
	var doc map[string]any
	if err := json.Unmarshal(specJSON, &doc); err != nil {
		return specJSON
	}
	paths, ok := doc["paths"].(map[string]any)
	if !ok {
		paths = map[string]any{}
		doc["paths"] = paths
	}
	for _, c := range comps {
		item, ok := paths[c.Path].(map[string]any)
		if !ok {
			item = map[string]any{}
			paths[c.Path] = item
		}
		item[strings.ToLower(c.Method)] = map[string]any{
			"operationId":       c.Name,
			"summary":           c.Summary,
			"x-wrapi-composite": true,
		}
	}
	if out, err := json.Marshal(doc); err == nil {
		return out
	}
	return specJSON
}
