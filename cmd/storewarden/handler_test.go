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
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"tailscale.com/client/tailscale/apitype"
	"tailscale.com/tailcfg"

	"github.com/nathan-scheele/storewarden/internal/authz"
	"github.com/nathan-scheele/storewarden/internal/shopify"
)

// Recognisable values, so a test can check they never leak into logs.
const (
	testClientID     = "test-client-id"
	testClientSecret = "test-secret-LEAKMARKER"
	testToken        = "shpat_test-token-LEAKMARKER"
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

// fakeShopify serves the client credentials grant and answers every GraphQL
// query with the response recorded in testdata.
func fakeShopify(t *testing.T) *httptest.Server {
	t.Helper()
	recorded, err := os.ReadFile("testdata/shop.json")
	if err != nil {
		t.Fatal(err)
	}
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
		if r.Header.Get("X-Shopify-Access-Token") != testToken {
			http.Error(w, "bad token", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(recorded)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
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
	url  string
	logs *syncBuffer
}

// newHarness starts the assembled server handler with the given WhoIs, a
// fake Shopify and debug-level JSON logs captured in memory.
func newHarness(t *testing.T, whois authz.WhoIser) *harness {
	t.Helper()
	logs := &syncBuffer{}
	logger := slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	client := shopify.New(fakeShopify(t).URL, testClientID, testClientSecret, nil)
	srv := httptest.NewServer(newHandler(whois, client, logger))
	t.Cleanup(srv.Close)
	return &harness{url: srv.URL, logs: logs}
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
