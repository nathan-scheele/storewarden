package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nathan-scheele/storewarden/internal/shopify"
)

// getInventoryQuery has a maximum query cost of 30.
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
						Quantities []struct {
							Name     string `json:"name"`
							Quantity int    `json:"quantity"`
						} `json:"quantities"`
					} `json:"nodes"`
				} `json:"inventoryLevels"`
			} `json:"inventoryItem"`
		}
		variables := map[string]any{"id": in.InventoryItemID}
		if _, err := client.Query(ctx, getInventoryQuery, variables, &data); err != nil {
			return nil, Inventory{}, err
		}
		// Shopify answers null for an ID it doesn't know.
		if data.InventoryItem == nil {
			return nil, Inventory{}, fmt.Errorf("no inventory item with ID %s", in.InventoryItemID)
		}
		out := Inventory{Levels: []InventoryLevel{}}
		for _, level := range data.InventoryItem.InventoryLevels.Nodes {
			l := InventoryLevel{LocationID: level.Location.ID, LocationName: level.Location.Name}
			for _, q := range level.Quantities {
				switch q.Name {
				case "available":
					l.Available = q.Quantity
				case "on_hand":
					l.OnHand = q.Quantity
				}
			}
			out.Levels = append(out.Levels, l)
		}
		return nil, out, nil
	}
}
