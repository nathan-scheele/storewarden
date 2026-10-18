package main

// The "tools" and "read-only" commands: Part 2 of the smoke test. "tools"
// runs as a Caller granted every Tool. It calls each Read tool, then takes
// one inventory item through a Dry run, a real write, a retry with the same
// Idempotency key and a write with a stale Expected quantity, and finally
// puts the quantity back. "read-only" runs as a Caller granted only the Read
// tools, and checks that the Write tool is hidden and denied.

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nathan-scheele/storewarden/internal/tools"
)

const writeTool = "set_inventory_quantity"

// staleText is how the Write tool's stale-quantity error starts.
const staleText = "the available quantity is no longer expected_quantity"

// scenario is what one command saw: its checks, and what the wizard needs
// to show the human and to check the Audit records.
type scenario struct {
	Steps []step `json:"steps"`
	Error string `json:"error,omitempty"` // why the scenario stopped early

	ShopName     string `json:"shop_name,omitempty"`
	ProductID    string `json:"product_id,omitempty"`
	ProductTitle string `json:"product_title,omitempty"`
	VariantID    string `json:"variant_id,omitempty"`
	ItemID       string `json:"item_id,omitempty"`
	LocationID   string `json:"location_id,omitempty"`
	Before       int    `json:"before"` // the available quantity before the write

	// The Audit request IDs of the real write and of putting it back.
	ExecutedID string `json:"executed_request_id,omitempty"`
	RestoredID string `json:"restored_request_id,omitempty"`
}

// step is one check: what was checked, whether it passed, and what was seen.
type step struct {
	Check  string `json:"check"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail,omitempty"`
}

func (sc *scenario) record(check string, pass bool, detail string) {
	sc.Steps = append(sc.Steps, step{check, pass, detail})
}

// failed records check as failed if the call failed, and reports whether it
// did. toolErr and err are what session.call returned: a Tool error and a
// failed call both count.
func (sc *scenario) failed(check, toolErr string, err error) bool {
	switch {
	case err != nil:
		sc.record(check, false, err.Error())
	case toolErr != "":
		sc.record(check, false, "Tool error: "+toolErr)
	default:
		return false
	}
	return true
}

// session is one MCP connection to storewarden.
type session struct{ cs *mcp.ClientSession }

// call calls tool with args. A Tool error comes back as its text in
// toolErr; otherwise out gets the Tool's structured result. err means the
// call couldn't be made at all, for example because storewarden doesn't
// serve the Tool to this Caller.
func (s session) call(ctx context.Context, tool string, args, out any) (toolErr string, err error) {
	res, err := s.cs.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return "", err
	}
	if res.IsError {
		var texts []string
		for _, c := range res.Content {
			if t, ok := c.(*mcp.TextContent); ok {
				texts = append(texts, t.Text)
			}
		}
		return strings.Join(texts, " "), nil
	}
	// StructuredContent arrives as generic JSON values. Encoding it again
	// and decoding it into out gives it out's type.
	b, err := json.Marshal(res.StructuredContent)
	if err != nil {
		return "", err
	}
	return "", json.Unmarshal(b, out)
}

// toolNames returns the names tools/list shows this Caller.
func (s session) toolNames(ctx context.Context) ([]string, error) {
	list, err := s.cs.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, t := range list.Tools {
		names = append(names, t.Name)
	}
	return names, nil
}

// available returns the available quantity of item at location.
func (s session) available(ctx context.Context, item, location string) (int, error) {
	var inv tools.Inventory
	toolErr, err := s.call(ctx, "get_inventory", tools.GetInventoryInput{InventoryItemID: item}, &inv)
	if err != nil {
		return 0, err
	}
	if toolErr != "" {
		return 0, fmt.Errorf("get_inventory: %s", toolErr)
	}
	for _, level := range inv.Levels {
		if level.LocationID == location {
			return level.Available, nil
		}
	}
	return 0, fmt.Errorf("get_inventory shows no level at %s", location)
}

// runTools is the "tools" command.
func runTools(ctx context.Context, s session) scenario {
	sc := scenario{Steps: []step{}}

	names, err := s.toolNames(ctx)
	if err != nil {
		sc.Error = "tools/list: " + err.Error()
		return sc
	}
	sc.record("tools/list shows all five Tools to the fully granted Caller",
		len(names) == 5 && slices.Contains(names, writeTool), strings.Join(names, ", "))

	var shop tools.Shop
	if toolErr, err := s.call(ctx, "get_shop", nil, &shop); sc.failed("get_shop", toolErr, err) {
		return sc
	}
	sc.ShopName = shop.Name

	// Two products a page, so that the dev store has a second page.
	first := 2
	var page1, page2 tools.ProductSearch
	const searched = "search_products returns a page of products"
	if toolErr, err := s.call(ctx, "search_products", tools.SearchProductsInput{First: &first}, &page1); sc.failed(searched, toolErr, err) {
		return sc
	}
	sc.record(searched, len(page1.Products) > 0, fmt.Sprintf("%d products", len(page1.Products)))
	if page1.NextCursor == "" {
		sc.record("search_products' next_cursor gets the next page", false, "no next_cursor: the Store has 2 products or fewer")
	} else {
		const paged = "search_products' next_cursor gets the next page"
		in := tools.SearchProductsInput{First: &first, After: page1.NextCursor}
		if toolErr, err := s.call(ctx, "search_products", in, &page2); !sc.failed(paged, toolErr, err) {
			sc.record(paged, len(page2.Products) > 0 && page2.Products[0].ID != page1.Products[0].ID,
				fmt.Sprintf("%d products", len(page2.Products)))
		}
	}

	// Find an inventory item stocked at some location, product by product.
	gotProduct := false
	var level tools.InventoryLevel
search:
	for _, summary := range append(page1.Products, page2.Products...) {
		var product tools.Product
		const got = "get_product returns variants with inventory item IDs"
		if toolErr, err := s.call(ctx, "get_product", tools.GetProductInput{ID: summary.ID}, &product); sc.failed(got, toolErr, err) {
			return sc
		}
		if !gotProduct {
			sc.record(got, len(product.Variants) > 0 && product.Variants[0].InventoryItemID != "",
				fmt.Sprintf("%d variants", len(product.Variants)))
			gotProduct = true
		}
		for _, v := range product.Variants {
			var inv tools.Inventory
			const read = "get_inventory returns quantities by location"
			if toolErr, err := s.call(ctx, "get_inventory", tools.GetInventoryInput{InventoryItemID: v.InventoryItemID}, &inv); sc.failed(read, toolErr, err) {
				return sc
			}
			if len(inv.Levels) > 0 {
				level = inv.Levels[0]
				sc.record(read, true, fmt.Sprintf("%d available, %d on hand at %s", level.Available, level.OnHand, level.LocationName))
				sc.ProductID, sc.ProductTitle, sc.VariantID = product.ID, product.Title, v.ID
				sc.ItemID, sc.LocationID, sc.Before = v.InventoryItemID, level.LocationID, level.Available
				break search
			}
		}
	}
	if sc.ItemID == "" {
		sc.Error = "no product on the first two pages is stocked at a location"
		return sc
	}

	before := sc.Before
	after := before + 1
	change := tools.SetInventoryQuantityInput{InventoryItemID: sc.ItemID, LocationID: sc.LocationID, Quantity: after}

	// A call with no dry_run is a Dry run.
	var dry tools.InventoryChange
	const dryRun = "a call without dry_run is a Dry run and changes nothing"
	if toolErr, err := s.call(ctx, writeTool, change, &dry); sc.failed(dryRun, toolErr, err) {
		return sc
	}
	now, err := s.available(ctx, sc.ItemID, sc.LocationID)
	sc.record(dryRun, err == nil && dry.DryRun && !dry.Applied && dry.Current == before && dry.New == after && now == before,
		fmt.Sprintf("dry_run=%t applied=%t current=%d new=%d, then available=%d %s", dry.DryRun, dry.Applied, dry.Current, dry.New, now, errText(err)))

	// The real write.
	execute := realWrite(change, newKey(), before)
	var done tools.InventoryChange
	const executed = "executing with a key and expected_quantity makes the change"
	if toolErr, err := s.call(ctx, writeTool, execute, &done); sc.failed(executed, toolErr, err) {
		return sc
	}
	sc.ExecutedID = done.RequestID
	now, err = s.available(ctx, sc.ItemID, sc.LocationID)
	sc.record(executed, err == nil && done.Applied && !done.DryRun && now == after,
		fmt.Sprintf("applied=%t, then available=%d %s; request_id %s", done.Applied, now, errText(err), done.RequestID))

	// The same call again, with the same key, must not apply twice.
	var again tools.InventoryChange
	toolErr, err := s.call(ctx, writeTool, execute, &again)
	seen := "returned the original result"
	if toolErr != "" {
		seen = "Tool error: " + toolErr
	}
	now, availErr := s.available(ctx, sc.ItemID, sc.LocationID)
	sc.record("retrying with the same key doesn't change the quantity again",
		err == nil && (toolErr == "" || strings.HasPrefix(toolErr, staleText)) && availErr == nil && now == after,
		fmt.Sprintf("%s; then available=%d %s", seen, now, errText(err, availErr)))

	// A new change based on the old read is refused.
	stale := realWrite(change, newKey(), before)
	stale.Quantity = after + 1
	var refused tools.InventoryChange
	toolErr, err = s.call(ctx, writeTool, stale, &refused)
	now, availErr = s.available(ctx, sc.ItemID, sc.LocationID)
	sc.record("a stale expected_quantity is refused with the stale-quantity error",
		err == nil && strings.HasPrefix(toolErr, staleText) && availErr == nil && now == after,
		fmt.Sprintf("Tool error %q; then available=%d %s", toolErr, now, errText(err, availErr)))

	// Put the quantity back.
	restore := realWrite(change, newKey(), after)
	restore.Quantity = before
	var restored tools.InventoryChange
	const putBack = "the quantity is put back"
	if toolErr, err := s.call(ctx, writeTool, restore, &restored); sc.failed(putBack, toolErr, err) {
		return sc
	}
	sc.RestoredID = restored.RequestID
	now, err = s.available(ctx, sc.ItemID, sc.LocationID)
	sc.record(putBack, err == nil && restored.Applied && now == before,
		fmt.Sprintf("available=%d %s; request_id %s", now, errText(err), restored.RequestID))
	return sc
}

// runReadOnly is the "read-only" command.
func runReadOnly(ctx context.Context, s session) scenario {
	sc := scenario{Steps: []step{}}

	names, err := s.toolNames(ctx)
	if err != nil {
		sc.Error = "tools/list: " + err.Error()
		return sc
	}
	sc.record("the read-only Caller's tools/list has the four Read tools and no Write tool",
		len(names) == 4 && !slices.Contains(names, writeTool), strings.Join(names, ", "))

	var shop tools.Shop
	const read = "the read-only Caller can use a Read tool"
	if toolErr, err := s.call(ctx, "get_shop", nil, &shop); !sc.failed(read, toolErr, err) {
		sc.record(read, shop.Name != "", "get_shop")
	}

	// A Dry run, so that nothing would change even if it were allowed.
	in := tools.SetInventoryQuantityInput{
		InventoryItemID: "gid://shopify/InventoryItem/1",
		LocationID:      "gid://shopify/Location/1",
	}
	var out tools.InventoryChange
	toolErr, err := s.call(ctx, writeTool, in, &out)
	want := fmt.Sprintf("unknown tool %q", writeTool)
	sc.record("the read-only Caller calling the Write tool gets "+want,
		err != nil && strings.Contains(err.Error(), want),
		fmt.Sprintf("error %q, Tool error %q", errText(err), toolErr))
	return sc
}

// realWrite returns change as a real write with key, expecting expected.
func realWrite(change tools.SetInventoryQuantityInput, key string, expected int) tools.SetInventoryQuantityInput {
	dryRun := false
	change.DryRun = &dryRun
	change.IdempotencyKey = key
	change.ExpectedQuantity = &expected
	return change
}

// newKey returns a new, random Idempotency key.
func newKey() string {
	return "smoke-" + rand.Text()
}

// errText joins the non-nil errors' text, for a step's detail.
func errText(errs ...error) string {
	var texts []string
	for _, err := range errs {
		if err != nil {
			texts = append(texts, err.Error())
		}
	}
	return strings.Join(texts, "; ")
}
