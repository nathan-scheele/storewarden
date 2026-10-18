package main

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"

	"github.com/nathan-scheele/storewarden/internal/authz"
)

// auditSink keeps the Audit records written to it, one JSON line per Write.
// failPhase makes every write of a record in that phase fail.
type auditSink struct {
	mu        sync.Mutex
	lines     []string
	failPhase string
}

func (s *auditSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rec struct{ Phase string }
	if err := json.Unmarshal(p, &rec); err == nil && rec.Phase == s.failPhase {
		return 0, errors.New("disk full")
	}
	s.lines = append(s.lines, string(p))
	return len(p), nil
}

// failOn makes writes of records in phase fail from now on.
func (s *auditSink) failOn(phase string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failPhase = phase
}

// records returns every Audit record written so far, decoded.
func (s *auditSink) records(t *testing.T) []map[string]any {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	recs := []map[string]any{}
	for _, line := range s.lines {
		if !strings.HasSuffix(line, "\n") {
			t.Errorf("audit record isn't a whole line: %q", line)
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("audit record isn't JSON: %v: %q", err, line)
		}
		recs = append(recs, rec)
	}
	return recs
}

func (s *auditSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.lines, "")
}

// taggedCaller is the Audit record identity of grantedTo's node.
var taggedCaller = map[string]any{
	"node_id":   "nTESTAGENT",
	"node_name": "agent.example.ts.net.",
	"tags":      []any{"tag:agent"},
	"user":      "tagged",
}

// checkFields fails the test if rec lacks any of want's fields or has a
// different value for one.
func checkFields(t *testing.T, rec, want map[string]any) {
	t.Helper()
	for k, v := range want {
		got, ok := rec[k]
		if !ok {
			t.Errorf("%s record has no %q field: %v", rec["phase"], k, rec)
			continue
		}
		gotJSON, _ := json.Marshal(got)
		wantJSON, _ := json.Marshal(v)
		if string(gotJSON) != string(wantJSON) {
			t.Errorf("%s record %q = %s, want %s", rec["phase"], k, gotJSON, wantJSON)
		}
	}
}

func TestAllowedCallWritesAttemptAndResultRecords(t *testing.T) {
	h := newHarness(t, grantedTo(`{"tools":["get_shop"]}`))
	callGetShop(t, h.connect(t))

	recs := h.audit.records(t)
	if len(recs) != 2 {
		t.Fatalf("got %d audit records, want attempt and result:\n%s", len(recs), h.audit)
	}
	attempt, result := recs[0], recs[1]
	common := map[string]any{
		"caller":          taggedCaller,
		"tool":            "get_shop",
		"args":            map[string]any{},
		"decision":        "allow",
		"dry_run":         false,
		"idempotency_key": "",
	}
	checkFields(t, attempt, common)
	checkFields(t, result, common)
	checkFields(t, attempt, map[string]any{"phase": "attempt", "outcome": "", "error": ""})
	checkFields(t, result, map[string]any{
		"phase":   "result",
		"outcome": "ok",
		"error":   "",
		"shopify": map[string]any{
			"requested_cost": 2,
			"actual_cost":    2,
			"request_id":     testShopifyRequestID,
		},
	})
	for _, rec := range recs {
		for _, field := range []string{"time", "request_id", "reason", "duration_ms"} {
			if _, ok := rec[field]; !ok {
				t.Errorf("%s record has no %q field", rec["phase"], field)
			}
		}
		if id, _ := rec["request_id"].(string); id == "" {
			t.Errorf("%s record has no request ID", rec["phase"])
		}
	}
	if attempt["request_id"] != result["request_id"] {
		t.Errorf("attempt and result request IDs differ: %v, %v", attempt["request_id"], result["request_id"])
	}
	if _, ok := attempt["shopify"]; ok {
		t.Errorf("attempt record has Shopify fields before Shopify was called: %v", attempt["shopify"])
	}
}

func TestForbiddenRequestWritesDenialRecord(t *testing.T) {
	h := newHarness(t, grantedTo()) // tagged, but no capability
	if got := h.postInitialize(t); got != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", got)
	}

	recs := h.audit.records(t)
	if len(recs) != 1 {
		t.Fatalf("got %d audit records, want one denial:\n%s", len(recs), h.audit)
	}
	checkFields(t, recs[0], map[string]any{
		"phase":    "denied",
		"caller":   taggedCaller,
		"decision": "deny",
		"reason":   "no github.com/nathan-scheele/storewarden capability",
	})
	if id, _ := recs[0]["request_id"].(string); id == "" {
		t.Error("denial record has no request ID")
	}
}

func TestUngrantedToolCallWritesDenialRecord(t *testing.T) {
	h := newHarness(t, grantedTo(`{"tools":[]}`))
	cs := h.connect(t)
	_, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "get_shop",
		Arguments: map[string]any{"note": "hello"},
	})
	if err == nil {
		t.Fatal("ungranted call succeeded")
	}

	recs := h.audit.records(t)
	if len(recs) != 1 {
		t.Fatalf("got %d audit records, want one denial:\n%s", len(recs), h.audit)
	}
	checkFields(t, recs[0], map[string]any{
		"phase":    "denied",
		"caller":   taggedCaller,
		"tool":     "get_shop",
		"args":     map[string]any{"note": "hello"},
		"decision": "deny",
		"reason":   "tool not granted",
	})
	if n := h.shopify.queryCount(); n != 0 {
		t.Errorf("Shopify got %d queries for a denied call", n)
	}
}

func TestUntaggedCallerIsRecordedByLogin(t *testing.T) {
	whois := fakeWhoIs(func() (*apitype.WhoIsResponse, error) {
		return &apitype.WhoIsResponse{
			Node: &tailcfg.Node{
				StableID: "nLAPTOP",
				Name:     "laptop.example.ts.net.",
			},
			UserProfile: &tailcfg.UserProfile{LoginName: "alice@example.com"},
			CapMap: tailcfg.PeerCapMap{
				authz.Capability: {`{"tools":["get_shop"]}`},
			},
		}, nil
	})
	h := newHarness(t, whois)
	callGetShop(t, h.connect(t))

	recs := h.audit.records(t)
	if len(recs) != 2 {
		t.Fatalf("got %d audit records, want attempt and result:\n%s", len(recs), h.audit)
	}
	want := map[string]any{
		"node_id":   "nLAPTOP",
		"node_name": "laptop.example.ts.net.",
		"tags":      nil,
		"user":      "alice@example.com",
	}
	for _, rec := range recs {
		checkFields(t, rec, map[string]any{"caller": want})
	}
}

func TestCallIsRefusedWhenAttemptCantBeRecorded(t *testing.T) {
	h := newHarness(t, grantedTo(`{"tools":["get_shop"]}`))
	h.audit.failOn("attempt")
	cs := h.connect(t)

	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_shop"})
	if err == nil && !res.IsError {
		t.Fatalf("call succeeded without an attempt record: %+v", res.StructuredContent)
	}
	if n := h.shopify.queryCount(); n != 0 {
		t.Errorf("Shopify got %d queries for an unaudited call", n)
	}
	if !h.logged(slog.LevelError, "writing audit record failed") {
		t.Errorf("no error logged for the failed write; logs:\n%s", h.logs)
	}
}

func TestLostResultRecordMakesHealthUnhealthy(t *testing.T) {
	h := newHarness(t, grantedTo(`{"tools":["get_shop"]}`))
	if got := h.health(t); got != http.StatusOK {
		t.Fatalf("health before any call = %d, want 200", got)
	}
	h.audit.failOn("result")

	// The call has happened by the time its result record is lost, so the
	// Caller still gets the result.
	callGetShop(t, h.connect(t))

	if !h.logged(slog.LevelError, "writing audit record failed") {
		t.Errorf("no error logged for the failed write; logs:\n%s", h.logs)
	}
	if got := h.health(t); got != http.StatusServiceUnavailable {
		t.Errorf("health after a lost result record = %d, want 503", got)
	}
}

// health returns the status code of GET /healthz.
func (h *harness) health(t *testing.T) int {
	t.Helper()
	resp, err := http.Get(h.url + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestRecordsHoldNoResponseBodiesOrCredentials(t *testing.T) {
	const marker = "RESPONSE-BODY-MARKER"
	h := newHarness(t, grantedTo(`{"tools":["get_shop"]}`))
	h.shopify.replaceInBody(`"storewarden-dev"`, `"`+marker+`"`)

	// The marker reaches the Caller, so it really was in the response.
	if got := callGetShop(t, h.connect(t))["name"]; got != marker {
		t.Fatalf("get_shop name = %v, want the marker", got)
	}
	for _, secret := range []string{marker, testClientSecret, testToken} {
		if strings.Contains(h.audit.String(), secret) {
			t.Errorf("audit records contain %q:\n%s", secret, h.audit)
		}
	}
}
