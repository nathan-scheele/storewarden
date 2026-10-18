package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

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

// WriteOptions are the inputs every Write tool takes. Every Write tool's
// input embeds them, and the Audit records show them.
type WriteOptions struct {
	// A pointer, so that a missing dry_run can mean true.
	DryRun         *bool  `json:"dry_run,omitempty" jsonschema:"true (the default) only reports the change; false makes it"`
	IdempotencyKey string `json:"idempotency_key,omitempty" jsonschema:"required when dry_run is false: up to 255 characters, new for each change and the same when retrying it; a UUID is a good choice"`
}

// isDryRun reports whether the call is a Dry run: every call is, unless the
// Caller says false.
func (o WriteOptions) isDryRun() bool {
	return o.DryRun == nil || *o.DryRun
}

// maxIdempotencyKeyLength is the longest Idempotency key a Caller may give.
const maxIdempotencyKeyLength = 255

// checkRealWrite returns an error unless in has what a real write needs.
func checkRealWrite(in SetInventoryQuantityInput) error {
	if strings.TrimSpace(in.IdempotencyKey) == "" {
		return errors.New("idempotency_key is required when dry_run is false")
	}
	if len(in.IdempotencyKey) > maxIdempotencyKeyLength {
		return fmt.Errorf("idempotency_key must be at most %d characters", maxIdempotencyKeyLength)
	}
	if in.ExpectedQuantity == nil {
		return errors.New("expected_quantity is required when dry_run is false; run a dry run to see the current quantity")
	}
	return nil
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
	if _, err := client.Query(ctx, getAvailableQuantityQuery, getAvailableQuantityMaxCost, variables, &data); err != nil {
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

// setAvailableQuantity makes a real write of in, which checkRealWrite has
// passed. Shopify's inventory history links the change back to this call's
// Audit records through the reference document URI.
func setAvailableQuantity(ctx context.Context, client *shopify.Client, in SetInventoryQuantityInput) error {
	variables := map[string]any{
		"inventoryItemId":      in.InventoryItemID,
		"locationId":           in.LocationID,
		"quantity":             in.Quantity,
		"expectedQuantity":     *in.ExpectedQuantity,
		"referenceDocumentUri": "storewarden://audit/" + audit.RequestID(ctx),
		"idempotencyKey":       in.IdempotencyKey,
	}
	// The Tool needs nothing from the response: Query returns Shopify's
	// user errors as an error, and no error means the change was made.
	var data struct{}
	_, err := client.Query(ctx, setAvailableQuantityMutation, setAvailableQuantityMaxCost, variables, &data)
	return plainWriteError(err)
}

// concurrentRequestWait is how long a Caller is told to wait before
// retrying a write that is still in progress. Shopify doesn't say; the
// first call usually finishes in well under this.
const concurrentRequestWait = 2 * time.Second

// plainWriteError turns Shopify's refusals of a real write, which come back
// as user errors with these codes, into errors that tell the Caller what to
// do next. The one refusal worth retrying unchanged is a retryableError.
// Any other error is returned as it is.
func plainWriteError(err error) error {
	var userErrs shopify.UserErrors
	if !errors.As(err, &userErrs) {
		return err
	}
	for _, ue := range userErrs {
		switch ue.Code {
		case "CHANGE_FROM_QUANTITY_STALE":
			return errors.New("the available quantity is no longer expected_quantity: it changed since you read it. " +
				"Nothing was changed. Run a dry run to see the current quantity, then make the change with that as expected_quantity and a new idempotency_key. " +
				"If you were retrying a change, the dry run also shows whether the earlier call already made it")
		case "IDEMPOTENCY_KEY_PARAMETER_MISMATCH":
			return errors.New("idempotency_key was already used with different input. " +
				"Nothing was changed. Use a new idempotency_key for each new change, and reuse a key only to retry exactly the same call")
		case "IDEMPOTENCY_CONCURRENT_REQUEST":
			return &retryableError{
				reason: "a call with this idempotency_key is still in progress, so this call changed nothing itself. " +
					"Keep the same idempotency_key: Shopify makes the change only once",
				wait: concurrentRequestWait,
			}
		}
	}
	return err
}
