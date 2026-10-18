package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nathan-scheele/storewarden/internal/shopify"
)

// getInventoryMaxCost is the most getInventoryQuery can cost.
const getInventoryMaxCost = 30

const getInventoryQuery = `query GetInventory($id: ID!) {
  inventoryItem(id: $id) {
    inventoryLevels(first: 100) {
      nodes {
        location {
          id
          name
        }
        quantities(names: ["available", "on_hand"]) {
          name
          quantity
        }
      }
    }
  }
}`

// GetInventoryInput is get_inventory's input.
type GetInventoryInput struct {
	InventoryItemID string `json:"inventory_item_id" jsonschema:"the inventory item's ID, such as gid://shopify/InventoryItem/123, from get_product"`
}

// Inventory is get_inventory's output.
type Inventory struct {
	Levels []InventoryLevel `json:"levels"`
}

// InventoryLevel is an inventory item's stock at one location.
type InventoryLevel struct {
	LocationID   string `json:"location_id"`
	LocationName string `json:"location_name"`
	Available    int    `json:"available" jsonschema:"units that can be sold"`
	OnHand       int    `json:"on_hand" jsonschema:"units physically at the location, whether or not they can be sold"`
}

// quantity is one of an inventory level's named quantities, as Shopify
// reports them.
type quantity struct {
	Name     string `json:"name"`
	Quantity int    `json:"quantity"`
}

// quantityNamed returns the quantity called name, and whether there is one.
func quantityNamed(quantities []quantity, name string) (int, bool) {
	for _, q := range quantities {
		if q.Name == name {
			return q.Quantity, true
		}
	}
	return 0, false
}

func getInventory(client *shopify.Client) mcp.ToolHandlerFor[GetInventoryInput, Inventory] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in GetInventoryInput) (*mcp.CallToolResult, Inventory, error) {
		if err := checkID("inventory_item_id", "InventoryItem", in.InventoryItemID); err != nil {
			return nil, Inventory{}, err
		}
		var data struct {
			InventoryItem *struct {
				InventoryLevels struct {
					Nodes []struct {
						Location struct {
							ID   string `json:"id"`
							Name string `json:"name"`
						} `json:"location"`
						Quantities []quantity `json:"quantities"`
					} `json:"nodes"`
				} `json:"inventoryLevels"`
			} `json:"inventoryItem"`
		}
		variables := map[string]any{"id": in.InventoryItemID}
		if _, err := client.Query(ctx, getInventoryQuery, getInventoryMaxCost, variables, &data); err != nil {
			return nil, Inventory{}, err
		}
		// Shopify answers null for an ID it doesn't know.
		if data.InventoryItem == nil {
			return nil, Inventory{}, fmt.Errorf("no inventory item with ID %s", in.InventoryItemID)
		}
		out := Inventory{Levels: []InventoryLevel{}}
		for _, level := range data.InventoryItem.InventoryLevels.Nodes {
			// A quantity Shopify leaves out counts as 0.
			available, _ := quantityNamed(level.Quantities, "available")
			onHand, _ := quantityNamed(level.Quantities, "on_hand")
			out.Levels = append(out.Levels, InventoryLevel{
				LocationID:   level.Location.ID,
				LocationName: level.Location.Name,
				Available:    available,
				OnHand:       onHand,
			})
		}
		return nil, out, nil
	}
}
