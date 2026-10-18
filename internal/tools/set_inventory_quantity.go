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

// setAvailableQuantityMaxCost is the most setAvailableQuantityMutation can
// cost.
const setAvailableQuantityMaxCost = 10

// setAvailableQuantityMutation makes a real write
// (docs/adr/0002-write-tools-only-where-shopify-enforces-idempotency.md).
// The quantity name and reason are fixed here, not taken from the Caller.
// $expectedQuantity is Int!, so changeFromQuantity is never null and
// Shopify always runs its compare-and-swap check. @idempotent hands the
// Caller's Idempotency key to Shopify, which applies a retry only once.
const setAvailableQuantityMutation = `mutation SetAvailableQuantity($inventoryItemId: ID!, $locationId: ID!, $quantity: Int!, $expectedQuantity: Int!, $referenceDocumentUri: String!, $idempotencyKey: String!) {
  inventorySetQuantities(
    input: {
      name: "available"
      reason: "correction"
      referenceDocumentUri: $referenceDocumentUri
      quantities: [
        {
          inventoryItemId: $inventoryItemId
          locationId: $locationId
          quantity: $quantity
          changeFromQuantity: $expectedQuantity
        }
      ]
    }
  ) @idempotent(key: $idempotencyKey) {
    inventoryAdjustmentGroup {
      id
    }
    userErrors {
      field
      message
      code
    }
  }
}`

// SetInventoryQuantityInput is set_inventory_quantity's input.
type SetInventoryQuantityInput struct {
	InventoryItemID string `json:"inventory_item_id" jsonschema:"the inventory item's ID, such as gid://shopify/InventoryItem/123, from get_product"`
	LocationID      string `json:"location_id" jsonschema:"the location's ID, such as gid://shopify/Location/123, from get_inventory"`
	Quantity        int    `json:"quantity" jsonschema:"the new available quantity, 0 or more"`
	// Embedded, so dry_run and idempotency_key are inputs of this Tool too.
	WriteOptions
	// A pointer, so that 0 differs from missing.
	ExpectedQuantity *int `json:"expected_quantity,omitempty" jsonschema:"required when dry_run is false: the available quantity you believe is current, from a dry run"`
}

// checkRealWrite returns an error unless in has what a real write needs.
func checkRealWrite(in SetInventoryQuantityInput) error {
	if err := in.checkKey(); err != nil {
		return err
	}
	return checkExpected("expected_quantity", in.ExpectedQuantity)
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
		if !in.isDryRun() {
			if err := checkRealWrite(in); err != nil {
				return nil, InventoryChange{}, err
			}
			if err := setAvailableQuantity(ctx, client, in); err != nil {
				return nil, InventoryChange{}, err
			}
			// Shopify's compare-and-swap check passed, so the quantity
			// before the change was the expected one.
			return nil, InventoryChange{
				DryRun:    false,
				Current:   *in.ExpectedQuantity,
				New:       in.Quantity,
				Applied:   true,
				RequestID: audit.RequestID(ctx),
			}, nil
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
	quantities, err := levelQuantities(ctx, client, getAvailableQuantityQuery, getAvailableQuantityMaxCost, item, location)
	if err != nil {
		return 0, err
	}
	available, ok := quantityNamed(quantities, "available")
	if !ok {
		return 0, errors.New("shopify reported no available quantity")
	}
	return available, nil
}

// setAvailableQuantity makes a real write of in, which checkRealWrite has
// passed. Shopify's inventory history links the change back to this call's
// Audit records through the reference document URI.
func setAvailableQuantity(ctx context.Context, client *shopify.Client, in SetInventoryQuantityInput) error {
	variables := map[string]any{
		"inventoryItemId":      in.InventoryItemID,
		"locationId":           in.LocationID,
		"quantity":             in.Quantity,
		"expectedQuantity":     *in.ExpectedQuantity,
		"referenceDocumentUri": auditURI(ctx),
		"idempotencyKey":       in.IdempotencyKey,
	}
	// The Tool needs nothing from the response: Query returns Shopify's
	// user errors as an error, and no error means the change was made.
	var data struct{}
	_, err := client.Query(ctx, setAvailableQuantityMutation, setAvailableQuantityMaxCost, variables, &data)
	return plainWriteError(err, staleAvailableQuantity)
}

// staleAvailableQuantity is set_inventory_quantity's wording for Shopify's
// CHANGE_FROM_QUANTITY_STALE.
const staleAvailableQuantity = "the available quantity is no longer expected_quantity: it changed since you read it. " +
	"Nothing was changed. Run a dry run to see the current quantity, then make the change with that as expected_quantity and a new idempotency_key. " +
	"If you were retrying a change, the dry run also shows whether the earlier call already made it"
