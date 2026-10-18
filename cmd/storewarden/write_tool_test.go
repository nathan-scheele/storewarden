package main

import (
	"encoding/json"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// inventoryWriter grants the Write tool and the Read tool a Caller uses
// before it.
const inventoryWriter = `{"tools":["get_inventory","set_inventory_quantity"]}`

// The inventory item and location the set_inventory_quantity recordings
// were made with. Shopify reported 50 available there.
const (
	testInventoryItem = "gid://shopify/InventoryItem/50260715896993"
	testLocation      = "gid://shopify/Location/85565997217"
)

func TestWriteToolIsAbsentWithoutWriteMode(t *testing.T) {
	h := newHarness(t, grantedTo(inventoryWriter))
	cs := h.connect(t)

	if got := toolNames(t, cs); !reflect.DeepEqual(got, []string{"get_inventory"}) {
		t.Errorf("tools/list = %v, want [get_inventory]", got)
	}
	_, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "set_inventory_quantity",
		Arguments: map[string]any{"inventory_item_id": testInventoryItem, "location_id": testLocation, "quantity": 49},
	})
	if want := `unknown tool "set_inventory_quantity"`; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want %s", err, want)
	}
	if n := h.store.queries.Load(); n != 0 {
		t.Errorf("Shopify got %d queries, want none", n)
	}
	// A policy that grants the Write tool is still a correct policy when
	// Write mode is off, so it mustn't warn on every request.
	if h.logged(slog.LevelWarn, "set_inventory_quantity") {
		t.Errorf("a Grant naming the Write tool was logged as unknown; logs:\n%s", h.logs)
	}
}

func TestWriteToolNeedsAGrantInWriteMode(t *testing.T) {
	h := newWriteHarness(t, grantedTo(readTools))
	cs := h.connect(t)

	if got := toolNames(t, cs); slices.Contains(got, "set_inventory_quantity") {
		t.Errorf("tools/list = %v, want no set_inventory_quantity", got)
	}
	_, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "set_inventory_quantity",
		Arguments: map[string]any{"inventory_item_id": testInventoryItem, "location_id": testLocation, "quantity": 49},
	})
	if want := `unknown tool "set_inventory_quantity"`; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want %s", err, want)
	}
	if n := h.store.queries.Load(); n != 0 {
		t.Errorf("Shopify got %d queries, want none", n)
	}
}

func TestWriteToolIsListedDestructiveAndIdempotent(t *testing.T) {
	cs := newWriteHarness(t, grantedTo(inventoryWriter)).connect(t)

	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var tool *mcp.Tool
	for _, tl := range res.Tools {
		if tl.Name == "set_inventory_quantity" {
			tool = tl
		}
	}
	if tool == nil {
		t.Fatalf("set_inventory_quantity not listed in Write mode with a Grant")
	}
	a := tool.Annotations
	if a == nil || a.ReadOnlyHint || !a.IdempotentHint || a.DestructiveHint == nil || !*a.DestructiveHint {
		t.Errorf("annotations = %+v, want destructive and idempotent, not read-only", a)
	}
}

// mutations returns the GraphQL mutations the fake Store has received.
func (s *fakeShopify) mutations() []graphQLRequest {
	var found []graphQLRequest
	for _, r := range s.requests() {
		if strings.HasPrefix(r.Query, "mutation") {
			found = append(found, r)
		}
	}
	return found
}

func TestSetInventoryQuantityIsADryRunByDefault(t *testing.T) {
	h := newWriteHarness(t, grantedTo(inventoryWriter))

	got := callTool(t, h.connect(t), "set_inventory_quantity", map[string]any{
		"inventory_item_id": testInventoryItem,
		"location_id":       testLocation,
		"quantity":          49,
	})
	recs := h.audit.records(t)
	if len(recs) == 0 {
		t.Fatal("no audit records")
	}
	want := map[string]any{
		"dry_run":    true,
		"current":    50.0, // as recorded
		"new":        49.0,
		"applied":    false,
		"request_id": recs[0]["request_id"],
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("set_inventory_quantity =\n%v\nwant\n%v", got, want)
	}
	if m := h.store.mutations(); len(m) != 0 {
		t.Errorf("Shopify received %d mutations in a dry run: %v", len(m), m)
	}
}

// testKey is the Idempotency key the tests' real writes use.
const testKey = "5a1d7e2b-8c3f-4a6e-9d0b-2c4e6f8a1b02"

// realWrite returns the arguments of a real write of 49 where 50 is
// expected.
func realWrite() map[string]any {
	return map[string]any{
		"inventory_item_id": testInventoryItem,
		"location_id":       testLocation,
		"quantity":          49,
		"dry_run":           false,
		"idempotency_key":   testKey,
		"expected_quantity": 50,
	}
}

func TestRealWriteNeedsAKeyAndAnExpectedQuantity(t *testing.T) {
	tests := map[string]struct {
		change func(args map[string]any)
		want   string
	}{
		"no key":          {func(a map[string]any) { delete(a, "idempotency_key") }, "idempotency_key is required when dry_run is false"},
		"empty key":       {func(a map[string]any) { a["idempotency_key"] = "" }, "idempotency_key is required when dry_run is false"},
		"blank key":       {func(a map[string]any) { a["idempotency_key"] = "  " }, "idempotency_key is required when dry_run is false"},
		"long key":        {func(a map[string]any) { a["idempotency_key"] = strings.Repeat("k", 256) }, "idempotency_key must be at most 255 characters"},
		"no expected":     {func(a map[string]any) { delete(a, "expected_quantity") }, "expected_quantity is required when dry_run is false"},
		"neither":         {func(a map[string]any) { delete(a, "idempotency_key"); delete(a, "expected_quantity") }, "idempotency_key is required when dry_run is false"},
		"negative amount": {func(a map[string]any) { a["quantity"] = -1 }, "quantity must be 0 or more"},
		// Taking a missing quantity as 0 would empty the location.
		"no quantity": {func(a map[string]any) { delete(a, "quantity") }, `missing properties: ["quantity"]`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h := newWriteHarness(t, grantedTo(inventoryWriter))
			args := realWrite()
			tt.change(args)

			got := callToolError(t, h.connect(t), "set_inventory_quantity", args)
			if !strings.Contains(got, tt.want) {
				t.Errorf("error = %q, want it to say %q", got, tt.want)
			}
			if n := h.store.queries.Load(); n != 0 {
				t.Errorf("Shopify got %d queries, want none", n)
			}
		})
	}
}

// answerMutation makes the fake Store answer the Write tool's mutation with
// the response recorded as name.
func (h *harness) answerMutation(t *testing.T, name string) {
	t.Helper()
	f := h.store.recorded(t, name)
	h.store.answer(f.query, f.response)
}

func TestRealWriteSendsTheKeyAndExpectedQuantityToShopify(t *testing.T) {
	h := newWriteHarness(t, grantedTo(inventoryWriter))
	h.answerMutation(t, "set_inventory_quantity_applied")

	got := callTool(t, h.connect(t), "set_inventory_quantity", realWrite())
	recs := h.audit.records(t)
	if len(recs) == 0 {
		t.Fatal("no audit records")
	}
	requestID := recs[0]["request_id"].(string)
	want := map[string]any{
		"dry_run":    false,
		"current":    50.0, // Shopify checked it was expected_quantity
		"new":        49.0,
		"applied":    true,
		"request_id": requestID,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("set_inventory_quantity =\n%v\nwant\n%v", got, want)
	}

	mutations := h.store.mutations()
	if len(mutations) != 1 {
		t.Fatalf("Shopify received %d mutations, want 1", len(mutations))
	}
	m := mutations[0]
	// The fixed parts of the change are in the mutation's text.
	for _, part := range []string{
		"inventorySetQuantities(",
		`name: "available"`,
		`reason: "correction"`,
		"referenceDocumentUri: $referenceDocumentUri",
		"changeFromQuantity: $expectedQuantity",
		"@idempotent(key: $idempotencyKey)",
		"$expectedQuantity: Int!", // never null, so the compare-and-swap check always runs
	} {
		if !strings.Contains(m.Query, part) {
			t.Errorf("mutation has no %q:\n%s", part, m.Query)
		}
	}
	wantVars := map[string]any{
		"inventoryItemId":      testInventoryItem,
		"locationId":           testLocation,
		"quantity":             49.0,
		"expectedQuantity":     50.0,
		"idempotencyKey":       testKey,
		"referenceDocumentUri": "storewarden://audit/" + requestID,
	}
	if !reflect.DeepEqual(m.Variables, wantVars) {
		t.Errorf("mutation variables =\n%v\nwant\n%v", m.Variables, wantVars)
	}
}

// setRefusal is NOT a recording. It answers a real write with a user error
// that couldn't be produced on the dev store, built from Shopify's
// documentation instead: code and message are as the 2026-10 schema lists
// them in InventorySetQuantitiesUserErrorCode. Several identical writes
// sent to the dev store at once never produced
// IDEMPOTENCY_CONCURRENT_REQUEST, and IDEMPOTENCY_PREVIOUS_ATTEMPT_FAILED
// was never seen: a move resent after a user error got that user error
// again. It is the recorded stale-quantity response with that one user
// error swapped in.
func setRefusal(t *testing.T, h *harness, code, message string) []byte {
	t.Helper()
	stale := h.store.recorded(t, "set_inventory_quantity_stale").response
	var resp map[string]any
	if err := json.Unmarshal(stale, &resp); err != nil {
		t.Fatal(err)
	}
	payload := resp["data"].(map[string]any)["inventorySetQuantities"].(map[string]any)
	payload["userErrors"] = []any{map[string]any{"field": nil, "message": message, "code": code}}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestShopifyRefusalsBecomePlainToolErrors(t *testing.T) {
	tests := map[string]struct {
		answer    func(t *testing.T, h *harness)
		want      []string
		retryable bool
	}{
		"stale quantity": {
			answer: func(t *testing.T, h *harness) { h.answerMutation(t, "set_inventory_quantity_stale") },
			want:   []string{"the available quantity is no longer expected_quantity", "Nothing was changed", "dry run"},
		},
		"key reused with different input": {
			answer: func(t *testing.T, h *harness) { h.answerMutation(t, "set_inventory_quantity_key_mismatch") },
			want:   []string{"idempotency_key was already used with different input", "Nothing was changed", "new idempotency_key"},
		},
		"concurrent request": {
			answer: func(t *testing.T, h *harness) {
				f := h.store.recorded(t, "set_inventory_quantity_applied")
				h.store.answer(f.query, setRefusal(t, h, "IDEMPOTENCY_CONCURRENT_REQUEST", "This request is currently in progress, please try again."))
			},
			// Worded like a rate-limited call: marked retryable, ending with a wait.
			want:      []string{"a call with this idempotency_key is still in progress", "same idempotency_key", "Retry the same call in 2s"},
			retryable: true,
		},
		"previous attempt failed": {
			answer: func(t *testing.T, h *harness) {
				f := h.store.recorded(t, "set_inventory_quantity_applied")
				h.store.answer(f.query, setRefusal(t, h, "IDEMPOTENCY_PREVIOUS_ATTEMPT_FAILED", "A previous request with this idempotency key failed. Retry with a new idempotency key."))
			},
			// The same wording as for move_inventory_quantity.
			want: []string{"an earlier call with this idempotency_key failed", "Nothing was changed", "Run a dry run", "new idempotency_key"},
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h := newWriteHarness(t, grantedTo(inventoryWriter))
			tt.answer(t, h)

			got := callToolError(t, h.connect(t), "set_inventory_quantity", realWrite())
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("error = %q, want it to say %q", got, w)
				}
			}
			if retryable := strings.HasPrefix(got, "retryable: "); retryable != tt.retryable {
				t.Errorf("error = %q, marked retryable: %v, want %v", got, retryable, tt.retryable)
			}
		})
	}
}

func TestDryRunAtALocationThatDoesNotStockTheItemIsAToolError(t *testing.T) {
	cs := newWriteHarness(t, grantedTo(inventoryWriter)).connect(t)

	got := callToolError(t, cs, "set_inventory_quantity", map[string]any{
		"inventory_item_id": testInventoryItem,
		"location_id":       "gid://shopify/Location/1",
		"quantity":          49,
	})
	want := "inventory item " + testInventoryItem + " isn't stocked at location gid://shopify/Location/1"
	if got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
}

func TestWriteAuditRecordsShowDryRunAndIdempotencyKey(t *testing.T) {
	dryRun := map[string]any{"inventory_item_id": testInventoryItem, "location_id": testLocation, "quantity": 49}
	// A key given with a Dry run is recorded too, as the Caller sent it.
	dryRunWithKey := map[string]any{"inventory_item_id": testInventoryItem, "location_id": testLocation, "quantity": 49, "idempotency_key": testKey}
	tests := map[string]struct {
		args map[string]any
		want map[string]any
	}{
		"dry run by default": {dryRun, map[string]any{"dry_run": true, "idempotency_key": ""}},
		"dry run with a key": {dryRunWithKey, map[string]any{"dry_run": true, "idempotency_key": testKey}},
		"real write":         {realWrite(), map[string]any{"dry_run": false, "idempotency_key": testKey}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h := newWriteHarness(t, grantedTo(inventoryWriter))
			h.answerMutation(t, "set_inventory_quantity_applied")
			callTool(t, h.connect(t), "set_inventory_quantity", tt.args)

			recs := h.audit.records(t)
			if len(recs) != 2 {
				t.Fatalf("got %d audit records, want attempt and result:\n%s", len(recs), h.audit)
			}
			for _, rec := range recs {
				checkFields(t, rec, tt.want)
			}
		})
	}
}

func TestRealWriteIsRefusedWhenAttemptCantBeRecorded(t *testing.T) {
	h := newWriteHarness(t, grantedTo(inventoryWriter))
	h.answerMutation(t, "set_inventory_quantity_applied")
	h.audit.failOn("attempt")

	res, err := h.connect(t).CallTool(t.Context(), &mcp.CallToolParams{Name: "set_inventory_quantity", Arguments: realWrite()})
	if err == nil && !res.IsError {
		t.Fatalf("write succeeded without an attempt record: %+v", res.StructuredContent)
	}
	if n := h.store.queries.Load(); n != 0 {
		t.Errorf("Shopify got %d queries for an unaudited write", n)
	}
}
