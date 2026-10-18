package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nathan-scheele/storewarden/internal/shopify"
)

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

// checkKey returns an error unless o has the Idempotency key a real write
// needs.
func (o WriteOptions) checkKey() error {
	if strings.TrimSpace(o.IdempotencyKey) == "" {
		return errors.New("idempotency_key is required when dry_run is false")
	}
	if len(o.IdempotencyKey) > maxIdempotencyKeyLength {
		return fmt.Errorf("idempotency_key must be at most %d characters", maxIdempotencyKeyLength)
	}
	return nil
}

// checkExpected returns an error unless a real write was given the Expected
// quantity in field. It is a pointer, so that 0 differs from missing.
func checkExpected(field string, expected *int) error {
	if expected == nil {
		return fmt.Errorf("%s is required when dry_run is false; run a dry run to see the current quantity", field)
	}
	return nil
}

// levelQuantities runs query, which reads an inventory item's named
// quantities at one location, and returns them. It is a Write tool's Dry
// run read; the query's variables are $inventoryItemId and $locationId.
func levelQuantities(ctx context.Context, client *shopify.Client, query string, maxCost float64, item, location string) ([]quantity, error) {
	var data struct {
		InventoryItem *struct {
			InventoryLevel *struct {
				Quantities []quantity `json:"quantities"`
			} `json:"inventoryLevel"`
		} `json:"inventoryItem"`
	}
	variables := map[string]any{"inventoryItemId": item, "locationId": location}
	if _, err := client.Query(ctx, query, maxCost, variables, &data); err != nil {
		return nil, err
	}
	// Shopify answers null for an ID it doesn't know, and for a location
	// that doesn't stock the item.
	if data.InventoryItem == nil {
		return nil, fmt.Errorf("no inventory item with ID %s", item)
	}
	if data.InventoryItem.InventoryLevel == nil {
		return nil, fmt.Errorf("inventory item %s isn't stocked at location %s", item, location)
	}
	return data.InventoryItem.InventoryLevel.Quantities, nil
}

// concurrentRequestWait is how long a Caller is told to wait before
// retrying a write that is still in progress. Shopify doesn't say; the
// first call usually finishes in well under this.
const concurrentRequestWait = 2 * time.Second

// plainWriteError turns Shopify's refusals of a real write, which come back
// as user errors with these codes, into errors that tell the Caller what to
// do next. stale is the Write tool's own wording for a failed
// compare-and-swap check, which names its Expected quantities. The one
// refusal worth retrying unchanged is a retryableError. Any other error is
// returned as it is.
func plainWriteError(err error, stale string) error {
	var userErrs shopify.UserErrors
	if !errors.As(err, &userErrs) {
		return err
	}
	for _, ue := range userErrs {
		switch ue.Code {
		case "CHANGE_FROM_QUANTITY_STALE":
			return errors.New(stale)
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
