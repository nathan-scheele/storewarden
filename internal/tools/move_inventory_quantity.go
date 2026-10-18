package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nathan-scheele/storewarden/internal/audit"
	"github.com/nathan-scheele/storewarden/internal/shopify"
)

// moveInventoryQuantityName is the move Tool's name. It needs the
// write_inventory Access scope, and its Dry run needs read_inventory.
const moveInventoryQuantityName = "move_inventory_quantity"

const moveInventoryQuantityDescription = "Move units of an inventory item at one location between available and damaged, quality_control or safety_stock. " +
	"Every call is a dry run that changes nothing and returns the current and new quantities of both states. " +
	"Real moves (dry_run false) are not supported yet."

// getMovableQuantitiesMaxCost is the most getMovableQuantitiesQuery can
// cost.
const getMovableQuantitiesMaxCost = 3

// getMovableQuantitiesQuery reads what a Dry run reports as current: every
// Inventory state a move can name, whichever two the Caller asked for, so
// that the query stays fixed.
const getMovableQuantitiesQuery = `query GetMovableQuantities($inventoryItemId: ID!, $locationId: ID!) {
  inventoryItem(id: $inventoryItemId) {
    inventoryLevel(locationId: $locationId) {
      quantities(names: ["available", "damaged", "quality_control", "safety_stock"]) {
        name
        quantity
      }
    }
  }
}`

// MoveInventoryQuantityInput is move_inventory_quantity's input.
type MoveInventoryQuantityInput struct {
	InventoryItemID string `json:"inventory_item_id" jsonschema:"the inventory item's ID, such as gid://shopify/InventoryItem/123, from get_product"`
	LocationID      string `json:"location_id" jsonschema:"the location's ID, such as gid://shopify/Location/123, from get_inventory"`
	From            string `json:"from" jsonschema:"the state to move units out of: available, damaged, quality_control or safety_stock; one of from and to must be available"`
	To              string `json:"to" jsonschema:"the state to move units into: available, damaged, quality_control or safety_stock; one of from and to must be available"`
	Quantity        int    `json:"quantity" jsonschema:"how many units to move, 1 or more"`
	// Embedded, so dry_run and idempotency_key are inputs of this Tool too.
	WriteOptions
	// Pointers, so that 0 differs from missing.
	ExpectedFromQuantity *int `json:"expected_from_quantity,omitempty" jsonschema:"required when dry_run is false: the quantity you believe is current in from, from a dry run"`
	ExpectedToQuantity   *int `json:"expected_to_quantity,omitempty" jsonschema:"required when dry_run is false: the quantity you believe is current in to, from a dry run"`
}

// InventoryMove is move_inventory_quantity's output.
type InventoryMove struct {
	DryRun    bool        `json:"dry_run"`
	From      StateChange `json:"from" jsonschema:"the state units move out of"`
	To        StateChange `json:"to" jsonschema:"the state units move into"`
	Applied   bool        `json:"applied" jsonschema:"whether the Store was changed"`
	RequestID string      `json:"request_id" jsonschema:"this call's ID in storewarden's audit log"`
}

// StateChange is one Inventory state's quantity before and after a move.
type StateChange struct {
	Current int `json:"current" jsonschema:"the quantity before the move"`
	New     int `json:"new" jsonschema:"the quantity after the move"`
}

// movableStates are the Inventory states a move can name. Reserved and
// committed stock is held for orders, incoming stock isn't at the location
// yet, and on hand is the sum of the others, so none of them is here.
var movableStates = []string{"available", "damaged", "quality_control", "safety_stock"}

// checkMove returns an error unless a move from one Inventory state to
// another is allowed: both are movable states, they differ, and exactly one
// of them is available.
func checkMove(from, to string) error {
	for _, s := range []struct{ field, state string }{{"from", from}, {"to", to}} {
		if !slices.Contains(movableStates, s.state) {
			return fmt.Errorf("%s must be available, damaged, quality_control or safety_stock, not %q", s.field, s.state)
		}
	}
	if from == to {
		return fmt.Errorf("from and to are both %s: a move needs two different states", from)
	}
	if from != "available" && to != "available" {
		return fmt.Errorf("one of from and to must be available: units move between available and damaged, quality_control or safety_stock, not from %s to %s", from, to)
	}
	return nil
}

func moveInventoryQuantity(client *shopify.Client) mcp.ToolHandlerFor[MoveInventoryQuantityInput, InventoryMove] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in MoveInventoryQuantityInput) (*mcp.CallToolResult, InventoryMove, error) {
		if err := checkID("inventory_item_id", "InventoryItem", in.InventoryItemID); err != nil {
			return nil, InventoryMove{}, err
		}
		if err := checkID("location_id", "Location", in.LocationID); err != nil {
			return nil, InventoryMove{}, err
		}
		if err := checkMove(in.From, in.To); err != nil {
			return nil, InventoryMove{}, err
		}
		if in.Quantity < 1 {
			return nil, InventoryMove{}, fmt.Errorf("quantity must be 1 or more, not %d", in.Quantity)
		}
		if !in.isDryRun() {
			return nil, InventoryMove{}, errRealMoveNotSupported
		}
		quantities, err := levelQuantities(ctx, client, getMovableQuantitiesQuery, getMovableQuantitiesMaxCost, in.InventoryItemID, in.LocationID)
		if err != nil {
			return nil, InventoryMove{}, err
		}
		from, ok := quantityNamed(quantities, in.From)
		if !ok {
			return nil, InventoryMove{}, fmt.Errorf("shopify reported no %s quantity", in.From)
		}
		to, ok := quantityNamed(quantities, in.To)
		if !ok {
			return nil, InventoryMove{}, fmt.Errorf("shopify reported no %s quantity", in.To)
		}
		return nil, InventoryMove{
			DryRun:    true,
			From:      StateChange{Current: from, New: from - in.Quantity},
			To:        StateChange{Current: to, New: to + in.Quantity},
			Applied:   false,
			RequestID: audit.RequestID(ctx),
		}, nil
	}
}

// errRealMoveNotSupported refuses a move with dry_run false. Real moves
// aren't built yet; the Dry run is.
var errRealMoveNotSupported = errors.New("dry_run false is not supported yet: move_inventory_quantity can only do a dry run for now, so nothing was changed")
