package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"

	"github.com/nathan-scheele/storewarden/internal/audit"
	"github.com/nathan-scheele/storewarden/internal/authz"
	"github.com/nathan-scheele/storewarden/internal/shopify"
)

// Recognisable values, so a test can check they never leak into logs.
const (
	testClientID     = "test-client-id"
	testClientSecret = "test-secret-LEAKMARKER"
	testToken        = "shpat_test-token-LEAKMARKER"

	testShopifyRequestID = "shopify-request-0001"

	// testQuotaShare is the harness's Quota share: storewarden's default.
	testQuotaShare = 0.5
)

// fakeWhoIs answers every WhoIs call the same way. A func type with a
// WhoIs method satisfies authz.WhoIser, so a test can pass a plain function.
type fakeWhoIs func() (*apitype.WhoIsResponse, error)

func (f fakeWhoIs) WhoIs(context.Context, string) (*apitype.WhoIsResponse, error) {
	return f()
}

// grantedTo returns a WhoIs answer for a tagged node whose matching
// Grants carry the given capability values, one per Grant.
func grantedTo(values ...string) fakeWhoIs {
	return func() (*apitype.WhoIsResponse, error) {
		resp := &apitype.WhoIsResponse{
			Node: &tailcfg.Node{
				StableID: "nTESTAGENT",
				Name:     "agent.example.ts.net.",
				Tags:     []string{"tag:agent"},
			},
			UserProfile: &tailcfg.UserProfile{LoginName: "tagged-devices"},
			CapMap:      tailcfg.PeerCapMap{},
		}
		for _, v := range values {
			resp.CapMap[authz.Capability] = append(resp.CapMap[authz.Capability], tailcfg.RawMessage(v))
		}
		return resp, nil
	}
}

// fixture is a response recorded from the dev store by
// scripts/record-fixture.sh, with the query and variables that produced it.
type fixture struct {
	name      string
	query     string
	variables map[string]any
	response  []byte
}

// loadFixtures reads every recorded response in testdata.
func loadFixtures(t *testing.T) []fixture {
	t.Helper()
	queries, err := filepath.Glob("testdata/*.graphql")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []fixture
	for _, q := range queries {
		name := strings.TrimSuffix(q, ".graphql")
		query, err := os.ReadFile(q)
		if err != nil {
			t.Fatal(err)
		}
		response, err := os.ReadFile(name + ".json")
		if err != nil {
			t.Fatal(err)
		}
		f := fixture{name: name, query: strings.TrimSpace(string(query)), response: response}
		// A query recorded without variables has no variables file.
		if vars, err := os.ReadFile(name + ".variables.json"); err == nil {
			if err := json.Unmarshal(vars, &f.variables); err != nil {
				t.Fatalf("%s.variables.json: %v", name, err)
			}
		}
		fixtures = append(fixtures, f)
	}
	return fixtures
}

// fakeShopify serves the client credentials grant and answers each GraphQL
// query with the response recorded for the same query and variables.
type fakeShopify struct {
	*httptest.Server
	queries atomic.Int64 // GraphQL requests received

	mu       sync.Mutex
	fixtures []fixture
	answers  map[string][]byte // query → the response to it, whatever the variables
	received []graphQLRequest
}

// graphQLRequest is one GraphQL request the fake Store received.
type graphQLRequest struct {
	Query     string
	Variables map[string]any
}

func newFakeShopify(t *testing.T) *fakeShopify {
	t.Helper()
	store := &fakeShopify{fixtures: loadFixtures(t)}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /admin/oauth/access_token", func(w http.ResponseWriter, r *http.Request) {
		if r.PostFormValue("grant_type") != "client_credentials" ||
			r.PostFormValue("client_id") != testClientID ||
			r.PostFormValue("client_secret") != testClientSecret {
			http.Error(w, "bad credentials", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"access_token": testToken,
			"scope":        "read_products",
			"expires_in":   86399,
		})
	})
	mux.HandleFunc("POST /admin/api/2026-10/graphql.json", func(w http.ResponseWriter, r *http.Request) {
		store.queries.Add(1)
		if r.Header.Get("X-Shopify-Access-Token") != testToken {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		query := strings.TrimSpace(req.Query)
		store.mu.Lock()
		store.received = append(store.received, graphQLRequest{Query: query, Variables: req.Variables})
		response := store.answers[query]
		for _, f := range store.fixtures {
			if response != nil {
				break
			}
			if query == f.query && reflect.DeepEqual(req.Variables, f.variables) {
				response = f.response
				break
			}
		}
		store.mu.Unlock()
		if response != nil {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Request-Id", testShopifyRequestID)
			w.Write(response)
			return
		}
		t.Errorf("no recorded response for query %q with variables %v", req.Query, req.Variables)
		http.Error(w, "no recorded response", http.StatusInternalServerError)
	})
	store.Server = httptest.NewServer(mux)
	t.Cleanup(store.Close)
	return store
}

// replaceInResponses changes the recorded responses before they are served,
// so a test can plant a recognisable value in them.
func (s *fakeShopify) replaceInResponses(old, new string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.fixtures {
		s.fixtures[i].response = bytes.ReplaceAll(s.fixtures[i].response, []byte(old), []byte(new))
	}
}

// recorded returns the fixture recorded as name, such as
// "set_inventory_quantity_stale".
func (s *fakeShopify) recorded(t *testing.T, name string) fixture {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, f := range s.fixtures {
		if f.name == filepath.Join("testdata", name) {
			return f
		}
	}
	t.Fatalf("no fixture named %s", name)
	return fixture{}
}

// answer makes the fake Store answer every request for query with
// response, whatever its variables. A mutation's variables hold the audit
// request ID, which is new on every call, so no recording matches them.
func (s *fakeShopify) answer(query string, response []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.answers == nil {
		s.answers = map[string][]byte{}
	}
	s.answers[strings.TrimSpace(query)] = response
}

// requests returns every GraphQL request the fake Store has received.
func (s *fakeShopify) requests() []graphQLRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.received)
}

// syncBuffer is a bytes.Buffer that is safe to write from server goroutines.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type harness struct {
	url   string
	logs  *syncBuffer
	audit *auditSink
	store *fakeShopify
}

// newHarness starts the assembled server handler with the given WhoIs, a
// fake Shopify, an in-memory audit sink and debug-level JSON logs captured
// in memory. Write mode is off.
func newHarness(t *testing.T, whois authz.WhoIser) *harness {
	t.Helper()
	return startHarness(t, whois, false)
}

// newWriteHarness is newHarness with Write mode on.
func newWriteHarness(t *testing.T, whois authz.WhoIser) *harness {
	t.Helper()
	return startHarness(t, whois, true)
}

func startHarness(t *testing.T, whois authz.WhoIser, writeMode bool) *harness {
	t.Helper()
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	store := newFakeShopify(t)
	client, err := shopify.New(store.URL, testClientID, testClientSecret, testQuotaShare, nil, logger)
	if err != nil {
		t.Fatal(err)
	}
	sink := &auditSink{}
	srv := httptest.NewServer(newHandler(whois, client, writeMode, audit.New(sink, logger), logger))
	t.Cleanup(srv.Close)
	return &harness{url: srv.URL, logs: logs, audit: sink, store: store}
}

func (h *harness) connect(t *testing.T) *mcp.ClientSession {
	t.Helper()
	client := mcp.NewClient(&mcp.Implementation{Name: "test-caller"}, nil)
	cs, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: h.url + "/mcp"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// logged reports whether a log entry at level mentions substr.
func (h *harness) logged(level slog.Level, substr string) bool {
	for line := range strings.Lines(h.logs.String()) {
		var entry struct{ Level string }
		if json.Unmarshal([]byte(line), &entry) == nil && entry.Level == level.String() && strings.Contains(line, substr) {
			return true
		}
	}
	return false
}

func toolNames(t *testing.T, cs *mcp.ClientSession) []string {
	t.Helper()
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func callGetShop(t *testing.T, cs *mcp.ClientSession) map[string]any {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_shop"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("get_shop returned a tool error: %+v", res.Content)
	}
	out, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content is %T, want an object", res.StructuredContent)
	}
	return out
}

func TestGrantedCallerSeesAndCallsGetShop(t *testing.T) {
	h := newHarness(t, grantedTo(`{"tools":["get_shop"]}`))
	cs := h.connect(t)

	if got := toolNames(t, cs); !reflect.DeepEqual(got, []string{"get_shop"}) {
		t.Errorf("tools/list = %v, want [get_shop]", got)
	}

	want := map[string]any{
		"name":             "storewarden-dev",
		"myshopify_domain": "storewarden-dev.myshopify.com",
		"plan":             "Plus",
		"currency":         "USD",
		"throttle_status": map[string]any{
			"currently_available": 19998.0,
			"maximum_available":   20000.0,
			"restore_rate":        1000.0,
		},
		// The share 0.5 of the Store's bucket. The Caller's Quota is new,
		// and a new Quota starts full.
		"quota": map[string]any{
			"available":    10000.0,
			"maximum":      10000.0,
			"restore_rate": 500.0,
		},
	}
	if got := callGetShop(t, cs); !reflect.DeepEqual(got, want) {
		t.Errorf("get_shop =\n%v\nwant\n%v", got, want)
	}
}

// postInitialize sends a raw MCP initialize request and returns the HTTP status.
func (h *harness) postInitialize(t *testing.T) int {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"raw","version":"0"}}}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, h.url+"/mcp", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestCallerWithoutCapabilityIsForbidden(t *testing.T) {
	tests := map[string]fakeWhoIs{
		"no capability": grantedTo(),
		"whois fails": func() (*apitype.WhoIsResponse, error) {
			return nil, errors.New("peer not found")
		},
	}
	for name, whois := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, whois)
			if got := h.postInitialize(t); got != http.StatusForbidden {
				t.Errorf("status = %d, want 403", got)
			}
		})
	}
}

func TestUngrantedToolIsHiddenAndUnknown(t *testing.T) {
	h := newHarness(t, grantedTo(`{"tools":[]}`))
	cs := h.connect(t)

	if got := toolNames(t, cs); len(got) != 0 {
		t.Errorf("tools/list = %v, want none", got)
	}

	_, denied := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "get_shop"})
	_, missing := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "no_such_tool"})
	if denied == nil || missing == nil {
		t.Fatalf("calls succeeded: denied err = %v, missing err = %v", denied, missing)
	}
	// A denied Tool must look exactly like one that doesn't exist.
	if want := strings.ReplaceAll(missing.Error(), "no_such_tool", "get_shop"); denied.Error() != want {
		t.Errorf("denied call error = %q, want %q", denied, want)
	}
}

func TestToolsAreTheUnionOfMatchingGrants(t *testing.T) {
	tests := map[string][]string{
		"granted first": {`{"tools":["get_shop"]}`, `{"tools":[]}`},
		"granted last":  {`{"tools":[]}`, `{"tools":["get_shop"]}`},
	}
	for name, values := range tests {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, grantedTo(values...))
			cs := h.connect(t)

			if got := toolNames(t, cs); !reflect.DeepEqual(got, []string{"get_shop"}) {
				t.Errorf("tools/list = %v, want [get_shop]", got)
			}
			callGetShop(t, cs)
		})
	}
}

func TestGrantWithUnknownFieldDeniesCaller(t *testing.T) {
	// The second value has a typo. The first value alone would grant get_shop.
	h := newHarness(t, grantedTo(`{"tools":["get_shop"]}`, `{"tool":["get_shop"]}`))

	if got := h.postInitialize(t); got != http.StatusForbidden {
		t.Errorf("status = %d, want 403", got)
	}
	if !h.logged(slog.LevelError, `unknown field \"tool\"`) {
		t.Errorf("no error logged for the unknown field; logs:\n%s", h.logs)
	}
}

func TestGrantNamingUnknownToolWarnsAndGrantsTheRest(t *testing.T) {
	h := newHarness(t, grantedTo(`{"tools":["get_shop","delete_everything"]}`))
	cs := h.connect(t)

	if got := toolNames(t, cs); !reflect.DeepEqual(got, []string{"get_shop"}) {
		t.Errorf("tools/list = %v, want [get_shop]", got)
	}
	callGetShop(t, cs)
	if !h.logged(slog.LevelWarn, "delete_everything") {
		t.Errorf("no warning logged for the unknown tool; logs:\n%s", h.logs)
	}
}

func TestShopifyCredentialsAreNeverLogged(t *testing.T) {
	h := newHarness(t, grantedTo(`{"tools":["get_shop"]}`))
	// Catch anything logged through the default logger too.
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(h.logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	callGetShop(t, h.connect(t))

	for _, secret := range []string{testClientSecret, testToken} {
		if strings.Contains(h.logs.String(), secret) {
			t.Errorf("logs contain %q", secret)
		}
	}
}

func TestHealthIsUnauthenticated(t *testing.T) {
	h := newHarness(t, grantedTo()) // a Caller with no capability at all
	resp, err := http.Get(h.url + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "healthy\n" {
		t.Errorf("GET /healthz = %d %q, want 200 \"healthy\\n\"", resp.StatusCode, body)
	}
}

func TestToolListIsPrivateToTheCaller(t *testing.T) {
	h := newHarness(t, grantedTo(`{"tools":["get_shop"]}`))
	res, err := h.connect(t).ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The list depends on the Caller's Grants, so nothing else may cache it.
	if res.CacheScope != "private" {
		t.Errorf("tools/list cacheScope = %q, want private", res.CacheScope)
	}
}
