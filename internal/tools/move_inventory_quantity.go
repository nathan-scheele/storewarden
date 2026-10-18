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
	"Every call is a dry run that changes nothing and returns the current and new quantities of both states, unless dry_run is false. " +
	"To make the move, call again with dry_run false, a new idempotency_key, and the dry run's current quantities as expected_from_quantity and expected_to_quantity. " +
	"Shopify rejects the move if either quantity is no longer expected, and applies a retry with the same idempotency_key only once."

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

// moveQuantityMaxCost is the most moveQuantityMutation can cost.
const moveQuantityMaxCost = 10

// moveQuantityMutation makes a real move
// (docs/adr/0002-write-tools-only-where-shopify-enforces-idempotency.md).
// Both Expected quantities are Int!, so neither changeFromQuantity is ever
// null and Shopify always runs both compare-and-swap checks. @idempotent
// hands the Caller's Idempotency key to Shopify, which applies a retry only
// once. Shopify wants a ledger document URI on the non-available side and
// refuses one on available, so the two ledger variables may be null; the
// Tool sets exactly one.
const moveQuantityMutation = `mutation MoveQuantity($inventoryItemId: ID!, $locationId: ID!, $quantity: Int!, $from: String!, $to: String!, $expectedFromQuantity: Int!, $expectedToQuantity: Int!, $reason: String!, $referenceDocumentUri: String!, $fromLedgerDocumentUri: String, $toLedgerDocumentUri: String, $idempotencyKey: String!) {
  inventoryMoveQuantities(
    input: {
      reason: $reason
      referenceDocumentUri: $referenceDocumentUri
      changes: [
        {
          inventoryItemId: $inventoryItemId
          quantity: $quantity
          from: {
            locationId: $locationId
            name: $from
            changeFromQuantity: $expectedFromQuantity
            ledgerDocumentUri: $fromLedgerDocumentUri
          }
          to: {
            locationId: $locationId
            name: $to
            changeFromQuantity: $expectedToQuantity
            ledgerDocumentUri: $toLedgerDocumentUri
          }
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

// checkRealMove returns an error unless in has what a real move needs: an
// Idempotency key, both Expected quantities, and no more units than the
// Caller expects the source state to hold.
func checkRealMove(in MoveInventoryQuantityInput) error {
	if err := in.checkKey(); err != nil {
		return err
	}
	if err := checkExpected("expected_from_quantity", in.ExpectedFromQuantity); err != nil {
		return err
	}
	if err := checkExpected("expected_to_quantity", in.ExpectedToQuantity); err != nil {
		return err
	}
	if in.Quantity > *in.ExpectedFromQuantity {
		return fmt.Errorf("quantity %d is more than expected_from_quantity %d, the units you expect %s to hold. "+
			"Nothing was changed. Run a dry run to see the current quantities, and move no more than from holds",
			in.Quantity, *in.ExpectedFromQuantity, in.From)
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
			if err := checkRealMove(in); err != nil {
				return nil, InventoryMove{}, err
			}
			if err := moveQuantity(ctx, client, in); err != nil {
				return nil, InventoryMove{}, err
			}
			// Shopify's compare-and-swap checks passed, so the quantities
			// before the move were the expected ones.
			from, to := *in.ExpectedFromQuantity, *in.ExpectedToQuantity
			return nil, InventoryMove{
				DryRun:    false,
				From:      StateChange{Current: from, New: from - in.Quantity},
				To:        StateChange{Current: to, New: to + in.Quantity},
				Applied:   true,
				RequestID: audit.RequestID(ctx),
			}, nil
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

// moveQuantity makes a real move of in, which checkRealMove has passed.
// The reason Shopify's inventory history shows is the non-available
// state's name, and the move links back to this call's Audit records
// through the reference document URI, which is also the ledger document
// URI on the non-available side.
func moveQuantity(ctx context.Context, client *shopify.Client, in MoveInventoryQuantityInput) error {
	uri := auditURI(ctx)
	variables := map[string]any{
		"inventoryItemId":       in.InventoryItemID,
		"locationId":            in.LocationID,
		"quantity":              in.Quantity,
		"from":                  in.From,
		"to":                    in.To,
		"expectedFromQuantity":  *in.ExpectedFromQuantity,
		"expectedToQuantity":    *in.ExpectedToQuantity,
		"referenceDocumentUri":  uri,
		"fromLedgerDocumentUri": nil,
		"toLedgerDocumentUri":   nil,
		"idempotencyKey":        in.IdempotencyKey,
	}
	// checkMove made sure exactly one side is available.
	if in.From == "available" {
		variables["reason"] = in.To
		variables["toLedgerDocumentUri"] = uri
	} else {
		variables["reason"] = in.From
		variables["fromLedgerDocumentUri"] = uri
	}
	// As for set_inventory_quantity, no error means the move was made.
	var data struct{}
	_, err := client.Query(ctx, moveQuantityMutation, moveQuantityMaxCost, variables, &data)
	return plainMoveError(err)
}

// staleMoveQuantities is move_inventory_quantity's wording for Shopify's
// CHANGE_FROM_QUANTITY_STALE, which it reports for either state or both.
const staleMoveQuantities = "a quantity changed since you read it: expected_from_quantity or expected_to_quantity is no longer current. " +
	"Nothing was changed. Run a dry run to see the current quantities, then move with those as expected_from_quantity and expected_to_quantity and a new idempotency_key. " +
	"If you were retrying a move, the dry run also shows whether the earlier call already made it"

// previousAttemptFailed is the wording for Shopify's
// IDEMPOTENCY_PREVIOUS_ATTEMPT_FAILED, which only the move Tool translates.
const previousAttemptFailed = "an earlier call with this idempotency_key failed, so Shopify won't use the key again. " +
	"Nothing was changed. Run a dry run again, then make the move with a new idempotency_key"

// plainMoveError is plainWriteError for a move, which also translates
// IDEMPOTENCY_PREVIOUS_ATTEMPT_FAILED.
func plainMoveError(err error) error {
	var userErrs shopify.UserErrors
	if errors.As(err, &userErrs) {
		for _, ue := range userErrs {
			if ue.Code == "IDEMPOTENCY_PREVIOUS_ATTEMPT_FAILED" {
				return errors.New(previousAttemptFailed)
			}
		}
	}
	return plainWriteError(err, staleMoveQuantities)
}
