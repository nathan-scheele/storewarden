package main

import (
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
