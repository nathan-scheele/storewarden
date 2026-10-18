package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// readTools grants every Read tool.
const readTools = `{"tools":["get_shop","search_products","get_product","get_inventory"]}`

// callTool calls a Tool that should succeed and returns its structured output.
func callTool(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) map[string]any {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("%s returned a tool error: %s", name, toolText(res))
	}
	out, ok := res.StructuredContent.(map[string]any)
	if !ok {
		t.Fatalf("structured content is %T, want an object", res.StructuredContent)
	}
	return out
}

// callToolError calls a Tool that should fail and returns its Tool error.
func callToolError(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) string {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("%s succeeded, want a tool error; output %v", name, res.StructuredContent)
	}
	return toolText(res)
}

// toolText returns the text of a Tool result's first content block.
func toolText(res *mcp.CallToolResult) string {
	if len(res.Content) == 0 {
		return ""
	}
	if text, ok := res.Content[0].(*mcp.TextContent); ok {
		return text.Text
	}
	return ""
}

func TestSearchProductsReturnsAPageAndItsCursor(t *testing.T) {
	cs := newHarness(t, grantedTo(readTools)).connect(t)

	got := callTool(t, cs, "search_products", map[string]any{"query": "tag:Snowboard", "first": 2})
	want := map[string]any{
		"products": []any{
			map[string]any{
				"id":              "gid://shopify/Product/8803713908897",
				"title":           "The Archived Snowboard",
				"handle":          "the-archived-snowboard",
				"status":          "ARCHIVED",
				"total_inventory": 50.0,
			},
			map[string]any{
				"id":              "gid://shopify/Product/8803713974433",
				"title":           "The Hidden Snowboard",
				"handle":          "the-hidden-snowboard",
				"status":          "ACTIVE",
				"total_inventory": 50.0,
			},
		},
		"next_cursor": "eyJsYXN0X2lkIjo4ODAzNzEzOTc0NDMzLCJsYXN0X3ZhbHVlIjo4ODAzNzEzOTc0NDMzfQ==",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("search_products =\n%v\nwant\n%v", got, want)
	}
}

func TestGetProductReturnsVariantsAndInventoryItems(t *testing.T) {
	cs := newHarness(t, grantedTo(readTools)).connect(t)

	got := callTool(t, cs, "get_product", map[string]any{"id": "gid://shopify/Product/8803714007201"})
	variant := func(id, title, item string) map[string]any {
		// The recorded variants have no SKU, so sku is absent.
		return map[string]any{"id": id, "title": title, "inventory_item_id": item}
	}
	want := map[string]any{
		"id":     "gid://shopify/Product/8803714007201",
		"title":  "The Complete Snowboard",
		"handle": "the-complete-snowboard",
		"status": "ACTIVE",
		"tags":   []any{"Premium", "Snow", "Snowboard", "Sport", "Winter"},
		"variants": []any{
			variant("gid://shopify/ProductVariant/48123931492513", "Ice", "gid://shopify/InventoryItem/50260716388513"),
			variant("gid://shopify/ProductVariant/48123931525281", "Dawn", "gid://shopify/InventoryItem/50260716421281"),
			variant("gid://shopify/ProductVariant/48123931558049", "Powder", "gid://shopify/InventoryItem/50260716454049"),
			variant("gid://shopify/ProductVariant/48123931590817", "Electric", "gid://shopify/InventoryItem/50260716486817"),
			variant("gid://shopify/ProductVariant/48123931623585", "Sunset", "gid://shopify/InventoryItem/50260716519585"),
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("get_product =\n%v\nwant\n%v", got, want)
	}
}

func TestGetProductThatDoesNotExistIsAToolError(t *testing.T) {
	cs := newHarness(t, grantedTo(readTools)).connect(t)

	got := callToolError(t, cs, "get_product", map[string]any{"id": "gid://shopify/Product/1"})
	if want := "no product with ID gid://shopify/Product/1"; got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
}

// badIDs are IDs every Tool that takes an ID of the given kind must reject.
func badIDs(kind string) []string {
	return []string{
		"",
		"123",
		"gid://shopify/" + kind + "/",
		"gid://shopify/" + kind + "/abc",
		"gid://shopify/" + kind + "/123/456",
		"gid://shopify/Location/123",
	}
}

func TestGetProductRejectsMalformedIDs(t *testing.T) {
	for _, id := range badIDs("Product") {
		h := newHarness(t, grantedTo(readTools))
		got := callToolError(t, h.connect(t), "get_product", map[string]any{"id": id})
		if want := "id must look like gid://shopify/Product/123"; !strings.Contains(got, want) {
			t.Errorf("id %q: error %q, want it to say %q", id, got, want)
		}
		if n := h.store.queries.Load(); n != 0 {
			t.Errorf("id %q: Shopify got %d queries, want none", id, n)
		}
	}
}

func TestSearchProductsRejectsPageSizeOutsideRange(t *testing.T) {
	for _, first := range []int{-1, 0, 51} {
		h := newHarness(t, grantedTo(readTools))
		got := callToolError(t, h.connect(t), "search_products", map[string]any{"query": "", "first": first})
		if want := "first must be from 1 to 50"; !strings.Contains(got, want) {
			t.Errorf("first = %d: error %q, want it to say %q", first, got, want)
		}
		if n := h.store.queries.Load(); n != 0 {
			t.Errorf("first = %d: Shopify got %d queries, want none", first, n)
		}
	}
}

func TestSearchProductsWithNoMatchesIsAnEmptyLastPage(t *testing.T) {
	cs := newHarness(t, grantedTo(readTools)).connect(t)

	// No page size, so the default of 10 is asked for.
	got := callTool(t, cs, "search_products", map[string]any{"query": "title:no-such-product"})
	want := map[string]any{"products": []any{}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("search_products = %v, want %v", got, want)
	}
}

func TestSearchProductsLastPageHasNoCursor(t *testing.T) {
	cs := newHarness(t, grantedTo(readTools)).connect(t)

	got := callTool(t, cs, "search_products", map[string]any{
		"query": "tag:Snowboard",
		"first": 50,
		"after": "eyJsYXN0X2lkIjo4ODAzNzEzOTc0NDMzLCJsYXN0X3ZhbHVlIjo4ODAzNzEzOTc0NDMzfQ==",
	})
	want := map[string]any{
		"products": []any{
			map[string]any{
				"id":              "gid://shopify/Product/8803714007201",
				"title":           "The Complete Snowboard",
				"handle":          "the-complete-snowboard",
				"status":          "ACTIVE",
				"total_inventory": 50.0,
			},
			map[string]any{
				"id":              "gid://shopify/Product/8803714138273",
				"title":           "The Multi-location Snowboard",
				"handle":          "the-multi-location-snowboard",
				"status":          "ACTIVE",
				"total_inventory": 100.0,
			},
			map[string]any{
				"id":              "gid://shopify/Product/8803714203809",
				"title":           "The Multi-managed Snowboard",
				"handle":          "the-multi-managed-snowboard",
				"status":          "ACTIVE",
				"total_inventory": 100.0,
			},
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("search_products =\n%v\nwant\n%v", got, want)
	}
}
