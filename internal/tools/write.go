package tools

import (
	"context"
	"errors"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nathan-scheele/storewarden/internal/audit"
	"github.com/nathan-scheele/storewarden/internal/shopify"
)

// setInventoryQuantityName is the Write tool's name. It needs the
// write_inventory Access scope, and its Dry run needs read_inventory.
const setInventoryQuantityName = "set_inventory_quantity"

const setInventoryQuantityDescription = "Set an inventory item's available quantity at one location. " +
	"Every call is a dry run that changes nothing and returns the current and new quantities, unless dry_run is false. " +
	"To make the change, call again with dry_run false, a new idempotency_key, and the current quantity from the dry run as expected_quantity. " +
	"Shopify rejects the change if the quantity is no longer expected_quantity, and applies a retry with the same idempotency_key only once."

// getAvailableQuantityMaxCost is the most getAvailableQuantityQuery can cost.
const getAvailableQuantityMaxCost = 3

// getAvailableQuantityQuery reads what a Dry run reports as current.
const getAvailableQuantityQuery = `query GetAvailableQuantity($inventoryItemId: ID!, $locationId: ID!) {
  inventoryItem(id: $inventoryItemId) {
    inventoryLevel(locationId: $locationId) {
      quantities(names: ["available"]) {
        name
        quantity
      }
    }
  }
}`

// SetInventoryQuantityInput is set_inventory_quantity's input.
type SetInventoryQuantityInput struct {
	InventoryItemID string `json:"inventory_item_id" jsonschema:"the inventory item's ID, such as gid://shopify/InventoryItem/123, from get_product"`
	LocationID      string `json:"location_id" jsonschema:"the location's ID, such as gid://shopify/Location/123, from get_inventory"`
	Quantity        int    `json:"quantity" jsonschema:"the new available quantity, 0 or more"`
	// A pointer, so that a missing dry_run can mean true.
	DryRun *bool `json:"dry_run,omitempty" jsonschema:"true (the default) only reports the change; false makes it"`
}

// InventoryChange is set_inventory_quantity's output.
type InventoryChange struct {
	DryRun    bool   `json:"dry_run"`
	Current   int    `json:"current" jsonschema:"the available quantity before the change"`
	New       int    `json:"new" jsonschema:"the available quantity after the change"`
	Applied   bool   `json:"applied" jsonschema:"whether the Store was changed"`
	RequestID string `json:"request_id" jsonschema:"this call's ID in storewarden's audit log"`
}

func setInventoryQuantity(client *shopify.Client) mcp.ToolHandlerFor[SetInventoryQuantityInput, InventoryChange] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in SetInventoryQuantityInput) (*mcp.CallToolResult, InventoryChange, error) {
		if err := checkID("inventory_item_id", "InventoryItem", in.InventoryItemID); err != nil {
			return nil, InventoryChange{}, err
		}
		if err := checkID("location_id", "Location", in.LocationID); err != nil {
			return nil, InventoryChange{}, err
		}
		if in.Quantity < 0 {
			return nil, InventoryChange{}, fmt.Errorf("quantity must be 0 or more, not %d", in.Quantity)
		}
		current, err := availableQuantity(ctx, client, in.InventoryItemID, in.LocationID)
		if err != nil {
			return nil, InventoryChange{}, err
		}
		return nil, InventoryChange{
			DryRun:    true,
			Current:   current,
			New:       in.Quantity,
			Applied:   false,
			RequestID: audit.RequestID(ctx),
		}, nil
	}
}

// availableQuantity returns an inventory item's available quantity at a
// location.
func availableQuantity(ctx context.Context, client *shopify.Client, item, location string) (int, error) {
	var data struct {
		InventoryItem *struct {
			InventoryLevel *struct {
				Quantities []struct {
					Name     string `json:"name"`
					Quantity int    `json:"quantity"`
				} `json:"quantities"`
			} `json:"inventoryLevel"`
		} `json:"inventoryItem"`
	}
	variables := map[string]any{"inventoryItemId": item, "locationId": location}
	if _, err := client.Query(ctx, getAvailableQuantityQuery, variables, &data); err != nil {
		return 0, err
	}
	// Shopify answers null for an ID it doesn't know, and for a location
	// that doesn't stock the item.
	if data.InventoryItem == nil {
		return 0, fmt.Errorf("no inventory item with ID %s", item)
	}
	if data.InventoryItem.InventoryLevel == nil {
		return 0, fmt.Errorf("inventory item %s isn't stocked at location %s", item, location)
	}
	for _, q := range data.InventoryItem.InventoryLevel.Quantities {
		if q.Name == "available" {
			return q.Quantity, nil
		}
	}
	return 0, errors.New("shopify reported no available quantity")
}
