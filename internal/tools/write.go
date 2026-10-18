package tools

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nathan-scheele/storewarden/internal/shopify"
)

// setInventoryQuantityName is the Write tool's name. It needs the
// write_inventory Access scope.
const setInventoryQuantityName = "set_inventory_quantity"

const setInventoryQuantityDescription = "Set an inventory item's available quantity at one location."

// SetInventoryQuantityInput is set_inventory_quantity's input.
type SetInventoryQuantityInput struct {
	InventoryItemID string `json:"inventory_item_id"`
	LocationID      string `json:"location_id"`
	Quantity        int    `json:"quantity"`
}

// InventoryChange is set_inventory_quantity's output.
type InventoryChange struct {
	DryRun bool `json:"dry_run"`
}

func setInventoryQuantity(client *shopify.Client) mcp.ToolHandlerFor[SetInventoryQuantityInput, InventoryChange] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in SetInventoryQuantityInput) (*mcp.CallToolResult, InventoryChange, error) {
		return nil, InventoryChange{}, errors.New("not yet")
	}
}
