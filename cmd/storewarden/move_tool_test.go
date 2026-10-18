package main

import (
	"encoding/json"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// inventoryMover grants the move Tool and the Read tool a Caller uses
// before it.
const inventoryMover = `{"tools":["get_inventory","move_inventory_quantity"]}`

// moveDryRun returns the arguments of a Dry run moving 3 units from
// available to damaged.
func moveDryRun() map[string]any {
	return map[string]any{
		"inventory_item_id": testInventoryItem,
		"location_id":       testLocation,
		"from":              "available",
		"to":                "damaged",
		"quantity":          3,
	}
}

func TestMoveToolIsAbsentWithoutWriteMode(t *testing.T) {
	h := newHarness(t, grantedTo(inventoryMover))
	cs := h.connect(t)

	if got := toolNames(t, cs); !reflect.DeepEqual(got, []string{"get_inventory"}) {
		t.Errorf("tools/list = %v, want [get_inventory]", got)
	}
	_, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "move_inventory_quantity", Arguments: moveDryRun()})
	if want := `unknown tool "move_inventory_quantity"`; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want %s", err, want)
	}
	if n := h.store.queries.Load(); n != 0 {
		t.Errorf("Shopify got %d queries, want none", n)
	}
	// A policy that grants the move Tool is still a correct policy when
	// Write mode is off, so it mustn't warn on every request.
	if h.logged(slog.LevelWarn, "move_inventory_quantity") {
		t.Errorf("a Grant naming the move Tool was logged as unknown; logs:\n%s", h.logs)
	}
}

func TestMoveToolNeedsItsOwnGrant(t *testing.T) {
	// set_inventory_quantity's Grant doesn't cover moves.
	h := newWriteHarness(t, grantedTo(inventoryWriter))
	cs := h.connect(t)

	if got := toolNames(t, cs); slices.Contains(got, "move_inventory_quantity") {
		t.Errorf("tools/list = %v, want no move_inventory_quantity", got)
	}
	_, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: "move_inventory_quantity", Arguments: moveDryRun()})
	if want := `unknown tool "move_inventory_quantity"`; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want %s", err, want)
	}
	if n := h.store.queries.Load(); n != 0 {
		t.Errorf("Shopify got %d queries, want none", n)
	}
	recs := h.audit.records(t)
	if len(recs) != 1 {
		t.Fatalf("got %d audit records, want one denial:\n%s", len(recs), h.audit)
	}
	checkFields(t, recs[0], map[string]any{"phase": "denied", "tool": "move_inventory_quantity", "reason": "tool not granted"})
}

func TestMoveToolIsListedDestructiveAndIdempotent(t *testing.T) {
	cs := newWriteHarness(t, grantedTo(inventoryMover)).connect(t)

	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var tool *mcp.Tool
	for _, tl := range res.Tools {
		if tl.Name == "move_inventory_quantity" {
			tool = tl
		}
	}
	if tool == nil {
		t.Fatalf("move_inventory_quantity not listed in Write mode with a Grant")
	}
	a := tool.Annotations
	if a == nil || a.ReadOnlyHint || !a.IdempotentHint || a.DestructiveHint == nil || !*a.DestructiveHint {
		t.Errorf("annotations = %+v, want destructive and idempotent, not read-only", a)
	}
}

// The move_inventory_quantity_current recording was made with 3 units moved
// to damaged, 2 to quality_control and 1 to safety_stock for the purpose
// (and moved back after), so each state has its own quantity: 44
// available, 3 damaged, 2 quality_control and 1 safety_stock.

func TestMoveIsADryRunByDefault(t *testing.T) {
	h := newWriteHarness(t, grantedTo(inventoryMover))

	got := callTool(t, h.connect(t), "move_inventory_quantity", moveDryRun())
	recs := h.audit.records(t)
	if len(recs) == 0 {
		t.Fatal("no audit records")
	}
	want := map[string]any{
		"dry_run":    true,
		"from":       map[string]any{"current": 44.0, "new": 41.0}, // available, as recorded
		"to":         map[string]any{"current": 3.0, "new": 6.0},   // damaged
		"applied":    false,
		"request_id": recs[0]["request_id"],
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("move_inventory_quantity =\n%v\nwant\n%v", got, want)
	}
	if m := h.store.mutations(); len(m) != 0 {
		t.Errorf("Shopify received %d mutations in a dry run: %v", len(m), m)
	}
}

func TestMoveDryRunReportsBothStates(t *testing.T) {
	tests := map[string]struct {
		from, to string
		quantity int
		wantFrom map[string]any
		wantTo   map[string]any
	}{
		"available to quality_control": {"available", "quality_control", 2, map[string]any{"current": 44.0, "new": 42.0}, map[string]any{"current": 2.0, "new": 4.0}},
		"available to safety_stock":    {"available", "safety_stock", 5, map[string]any{"current": 44.0, "new": 39.0}, map[string]any{"current": 1.0, "new": 6.0}},
		"damaged to available":         {"damaged", "available", 3, map[string]any{"current": 3.0, "new": 0.0}, map[string]any{"current": 44.0, "new": 47.0}},
		"quality_control to available": {"quality_control", "available", 1, map[string]any{"current": 2.0, "new": 1.0}, map[string]any{"current": 44.0, "new": 45.0}},
		"safety_stock to available":    {"safety_stock", "available", 1, map[string]any{"current": 1.0, "new": 0.0}, map[string]any{"current": 44.0, "new": 45.0}},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h := newWriteHarness(t, grantedTo(inventoryMover))
			args := moveDryRun()
			args["from"], args["to"], args["quantity"] = tt.from, tt.to, tt.quantity
			args["dry_run"] = true

			got := callTool(t, h.connect(t), "move_inventory_quantity", args)
			if !reflect.DeepEqual(got["from"], tt.wantFrom) || !reflect.DeepEqual(got["to"], tt.wantTo) {
				t.Errorf("from = %v, to = %v; want %v, %v", got["from"], got["to"], tt.wantFrom, tt.wantTo)
			}
			if got["dry_run"] != true || got["applied"] != false {
				t.Errorf("dry_run = %v, applied = %v; want true, false", got["dry_run"], got["applied"])
			}
			if m := h.store.mutations(); len(m) != 0 {
				t.Errorf("Shopify received %d mutations in a dry run: %v", len(m), m)
			}
		})
	}
}

func TestMoveDryRunAtALocationThatDoesNotStockTheItemIsAToolError(t *testing.T) {
	cs := newWriteHarness(t, grantedTo(inventoryMover)).connect(t)
	args := moveDryRun()
	args["location_id"] = "gid://shopify/Location/1"

	got := callToolError(t, cs, "move_inventory_quantity", args)
	want := "inventory item " + testInventoryItem + " isn't stocked at location gid://shopify/Location/1"
	if got != want {
		t.Errorf("error = %q, want %q", got, want)
	}
}

func TestMoveAuditRecords(t *testing.T) {
	h := newWriteHarness(t, grantedTo(inventoryMover))
	callTool(t, h.connect(t), "move_inventory_quantity", moveDryRun())

	recs := h.audit.records(t)
	if len(recs) != 2 {
		t.Fatalf("got %d audit records, want attempt and result:\n%s", len(recs), h.audit)
	}
	attempt, result := recs[0], recs[1]
	common := map[string]any{
		"caller":          taggedCaller,
		"tool":            "move_inventory_quantity",
		"args":            moveDryRun(),
		"decision":        "allow",
		"dry_run":         true,
		"idempotency_key": "",
	}
	checkFields(t, attempt, common)
	checkFields(t, result, common)
	checkFields(t, attempt, map[string]any{"phase": "attempt"})
	checkFields(t, result, map[string]any{
		"phase":   "result",
		"outcome": "ok",
		"error":   "",
		"shopify": map[string]any{
			"requested_cost": 3,
			"actual_cost":    3,
			"request_id":     testShopifyRequestID,
		},
	})
}

func TestMoveDryRunRecordsHoldNoResponseBodies(t *testing.T) {
	h := newWriteHarness(t, grantedTo(inventoryMover))
	// 44 is the recorded available quantity. The planted one is long
	// enough not to turn up anywhere else by chance.
	const planted = "4409182736"
	h.store.replaceInResponses(`"quantity": 44`, `"quantity": `+planted)

	got := callTool(t, h.connect(t), "move_inventory_quantity", moveDryRun())
	if from := got["from"].(map[string]any); from["current"] != 4409182736.0 {
		t.Fatalf("from = %v, want the planted current quantity", from)
	}
	if strings.Contains(h.audit.String(), planted) {
		t.Errorf("audit records contain Shopify's response:\n%s", h.audit)
	}
}

// realMove returns the arguments of a real move of 3 units from available
// to damaged, where 50 and 0 are expected.
func realMove() map[string]any {
	args := moveDryRun()
	args["dry_run"] = false
	args["idempotency_key"] = testKey
	args["expected_from_quantity"] = 50
	args["expected_to_quantity"] = 0
	return args
}

func TestRealMoveNeedsAKeyAndBothExpectedQuantities(t *testing.T) {
	tests := map[string]struct {
		change func(args map[string]any)
		want   string
	}{
		"no key":           {func(a map[string]any) { delete(a, "idempotency_key") }, "idempotency_key is required when dry_run is false"},
		"blank key":        {func(a map[string]any) { a["idempotency_key"] = "  " }, "idempotency_key is required when dry_run is false"},
		"long key":         {func(a map[string]any) { a["idempotency_key"] = strings.Repeat("k", 256) }, "idempotency_key must be at most 255 characters"},
		"no expected from": {func(a map[string]any) { delete(a, "expected_from_quantity") }, "expected_from_quantity is required when dry_run is false"},
		"no expected to":   {func(a map[string]any) { delete(a, "expected_to_quantity") }, "expected_to_quantity is required when dry_run is false"},
		"neither expected": {func(a map[string]any) { delete(a, "expected_from_quantity"); delete(a, "expected_to_quantity") }, "expected_from_quantity is required when dry_run is false"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h := newWriteHarness(t, grantedTo(inventoryMover))
			args := realMove()
			tt.change(args)

			got := callToolError(t, h.connect(t), "move_inventory_quantity", args)
			if !strings.Contains(got, tt.want) {
				t.Errorf("error = %q, want it to say %q", got, tt.want)
			}
			if n := h.store.queries.Load(); n != 0 {
				t.Errorf("Shopify got %d queries, want none", n)
			}
		})
	}
}

// The move_inventory_quantity_applied recording moved 3 units from
// available to damaged where 50 and 0 were expected, as realMove does.

func TestRealMoveIsMadeAndReportsBothStates(t *testing.T) {
	h := newWriteHarness(t, grantedTo(inventoryMover))
	h.answerMutation(t, "move_inventory_quantity_applied")

	got := callTool(t, h.connect(t), "move_inventory_quantity", realMove())
	recs := h.audit.records(t)
	if len(recs) == 0 {
		t.Fatal("no audit records")
	}
	want := map[string]any{
		"dry_run":    false,
		"from":       map[string]any{"current": 50.0, "new": 47.0}, // Shopify checked both were expected
		"to":         map[string]any{"current": 0.0, "new": 3.0},
		"applied":    true,
		"request_id": recs[0]["request_id"],
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("move_inventory_quantity =\n%v\nwant\n%v", got, want)
	}
	// Shopify checks the quantities itself, so nothing is read first.
	if n := h.store.queries.Load(); n != 1 {
		t.Errorf("Shopify got %d queries, want only the move", n)
	}
}

func TestRealMoveSendsBothExpectedQuantitiesTheKeyAndItsReferences(t *testing.T) {
	tests := map[string]struct {
		from, to string
		reason   string
		// Shopify takes a ledger document URI on the non-available side
		// only, and refuses one on available.
		fromLedger, toLedger bool
	}{
		"available to damaged":         {"available", "damaged", "damaged", false, true},
		"available to safety_stock":    {"available", "safety_stock", "safety_stock", false, true},
		"quality_control to available": {"quality_control", "available", "quality_control", true, false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h := newWriteHarness(t, grantedTo(inventoryMover))
			h.answerMutation(t, "move_inventory_quantity_applied")
			args := realMove()
			args["from"], args["to"] = tt.from, tt.to

			callTool(t, h.connect(t), "move_inventory_quantity", args)
			recs := h.audit.records(t)
			if len(recs) == 0 {
				t.Fatal("no audit records")
			}
			uri := "storewarden://audit/" + recs[0]["request_id"].(string)

			mutations := h.store.mutations()
			if len(mutations) != 1 {
				t.Fatalf("Shopify received %d mutations, want 1", len(mutations))
			}
			m := mutations[0]
			for _, part := range []string{
				"inventoryMoveQuantities(",
				"reason: $reason",
				"referenceDocumentUri: $referenceDocumentUri",
				"changeFromQuantity: $expectedFromQuantity",
				"changeFromQuantity: $expectedToQuantity",
				"@idempotent(key: $idempotencyKey)",
				// Never null, so both compare-and-swap checks always run.
				"$expectedFromQuantity: Int!",
				"$expectedToQuantity: Int!",
			} {
				if !strings.Contains(m.Query, part) {
					t.Errorf("mutation has no %q:\n%s", part, m.Query)
				}
			}
			ledger := func(on bool) any {
				if on {
					return uri
				}
				return nil
			}
			wantVars := map[string]any{
				"inventoryItemId":       testInventoryItem,
				"locationId":            testLocation,
				"quantity":              3.0,
				"from":                  tt.from,
				"to":                    tt.to,
				"expectedFromQuantity":  50.0,
				"expectedToQuantity":    0.0,
				"reason":                tt.reason,
				"referenceDocumentUri":  uri,
				"fromLedgerDocumentUri": ledger(tt.fromLedger),
				"toLedgerDocumentUri":   ledger(tt.toLedger),
				"idempotencyKey":        testKey,
			}
			if !reflect.DeepEqual(m.Variables, wantVars) {
				t.Errorf("mutation variables =\n%v\nwant\n%v", m.Variables, wantVars)
			}
		})
	}
}

func TestRealMoveAuditRecords(t *testing.T) {
	h := newWriteHarness(t, grantedTo(inventoryMover))
	// The adjustment group's ID is only in Shopify's response.
	const planted = "AdjustmentGroup/9182736450"
	h.store.replaceInResponses("AdjustmentGroup/49286662914209", planted)
	h.answerMutation(t, "move_inventory_quantity_applied")
	if !strings.Contains(string(h.store.recorded(t, "move_inventory_quantity_applied").response), planted) {
		t.Fatal("the planted ID isn't in the response Shopify will send")
	}

	callTool(t, h.connect(t), "move_inventory_quantity", realMove())
	recs := h.audit.records(t)
	if len(recs) != 2 {
		t.Fatalf("got %d audit records, want attempt and result:\n%s", len(recs), h.audit)
	}
	attempt, result := recs[0], recs[1]
	// args holds both Inventory states, the quantity and both Expected
	// quantities, as the Caller sent them.
	common := map[string]any{
		"tool":            "move_inventory_quantity",
		"args":            realMove(),
		"decision":        "allow",
		"dry_run":         false,
		"idempotency_key": testKey,
	}
	checkFields(t, attempt, common)
	checkFields(t, result, common)
	checkFields(t, result, map[string]any{
		"phase":   "result",
		"outcome": "ok",
		"error":   "",
		"shopify": map[string]any{
			"requested_cost": 10,
			"actual_cost":    10,
			"request_id":     testShopifyRequestID,
		},
	})
	if strings.Contains(h.audit.String(), planted) {
		t.Errorf("audit records contain Shopify's response:\n%s", h.audit)
	}
}

func TestRealMoveIsRefusedWhenAttemptCantBeRecorded(t *testing.T) {
	h := newWriteHarness(t, grantedTo(inventoryMover))
	h.answerMutation(t, "move_inventory_quantity_applied")
	h.audit.failOn("attempt")

	res, err := h.connect(t).CallTool(t.Context(), &mcp.CallToolParams{Name: "move_inventory_quantity", Arguments: realMove()})
	if err == nil && !res.IsError {
		t.Fatalf("move succeeded without an attempt record: %+v", res.StructuredContent)
	}
	if n := h.store.queries.Load(); n != 0 {
		t.Errorf("Shopify got %d queries for an unaudited move", n)
	}
}

// moveRefusal is NOT a recording. It answers a real move with a user error
// that couldn't be produced on the dev store, built from Shopify's
// documentation instead: code and message are as the 2026-10 schema lists
// them in InventoryMoveQuantitiesUserErrorCode. It is the recorded
// stale-move response with that one user error swapped in.
func moveRefusal(t *testing.T, h *harness, code, message string) []byte {
	t.Helper()
	stale := h.store.recorded(t, "move_inventory_quantity_stale").response
	var resp map[string]any
	if err := json.Unmarshal(stale, &resp); err != nil {
		t.Fatal(err)
	}
	payload := resp["data"].(map[string]any)["inventoryMoveQuantities"].(map[string]any)
	payload["userErrors"] = []any{map[string]any{"field": nil, "message": message, "code": code}}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestShopifyRefusalsOfAMoveBecomePlainToolErrors(t *testing.T) {
	tests := map[string]struct {
		answer    func(t *testing.T, h *harness)
		want      []string
		retryable bool
		// What the result record shows, without Shopify's messages.
		recorded string
	}{
		"stale quantity": {
			answer: func(t *testing.T, h *harness) { h.answerMutation(t, "move_inventory_quantity_stale") },
			want: []string{"a quantity changed since you read it", "expected_from_quantity", "expected_to_quantity",
				"Nothing was changed", "Run a dry run", "new idempotency_key"},
			recorded: "a quantity changed since you read it",
		},
		"key reused with different input": {
			answer:   func(t *testing.T, h *harness) { h.answerMutation(t, "move_inventory_quantity_key_mismatch") },
			want:     []string{"idempotency_key was already used with different input", "Nothing was changed", "new idempotency_key"},
			recorded: "idempotency_key was already used with different input",
		},
		// Identical calls sent at once couldn't produce this for
		// set_inventory_quantity, so it isn't tried for moves.
		"concurrent request": {
			answer: func(t *testing.T, h *harness) {
				f := h.store.recorded(t, "move_inventory_quantity_applied")
				h.store.answer(f.query, moveRefusal(t, h, "IDEMPOTENCY_CONCURRENT_REQUEST", "This request is currently in progress, please try again."))
			},
			// The same form as for set_inventory_quantity.
			want:      []string{"a call with this idempotency_key is still in progress", "same idempotency_key", "Retry the same call in 2s"},
			retryable: true,
			recorded:  "retryable: a call with this idempotency_key is still in progress",
		},
		// Retrying a move that failed with a user error, with the same key
		// and input, got that user error again on the dev store, not this.
		"previous attempt failed": {
			answer: func(t *testing.T, h *harness) {
				f := h.store.recorded(t, "move_inventory_quantity_applied")
				h.store.answer(f.query, moveRefusal(t, h, "IDEMPOTENCY_PREVIOUS_ATTEMPT_FAILED", "A previous request with this idempotency key failed. Retry with a new idempotency key."))
			},
			want:     []string{"an earlier call with this idempotency_key failed", "Nothing was changed", "Run a dry run", "new idempotency_key"},
			recorded: "an earlier call with this idempotency_key failed",
		},
		"any other refusal": {
			answer: func(t *testing.T, h *harness) {
				f := h.store.recorded(t, "move_inventory_quantity_invalid_location")
				h.store.answer(f.query, f.response)
			},
			// Passed through, as for set_inventory_quantity: the Caller
			// gets Shopify's message, the result record its codes.
			want:     []string{"The specified location could not be found."},
			recorded: "shopify: user errors, codes: INVALID_LOCATION, INVALID_LOCATION",
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h := newWriteHarness(t, grantedTo(inventoryMover))
			tt.answer(t, h)

			got := callToolError(t, h.connect(t), "move_inventory_quantity", realMove())
			for _, w := range tt.want {
				if !strings.Contains(got, w) {
					t.Errorf("error = %q, want it to say %q", got, w)
				}
			}
			if retryable := strings.HasPrefix(got, "retryable: "); retryable != tt.retryable {
				t.Errorf("error = %q, marked retryable: %v, want %v", got, retryable, tt.retryable)
			}

			recs := h.audit.records(t)
			if len(recs) != 2 {
				t.Fatalf("got %d audit records, want attempt and result:\n%s", len(recs), h.audit)
			}
			checkFields(t, recs[1], map[string]any{"outcome": "error"})
			if gotRec, _ := recs[1]["error"].(string); !strings.Contains(gotRec, tt.recorded) {
				t.Errorf("result error = %q, want it to say %q", gotRec, tt.recorded)
			}
			for _, message := range []string{"persisted quantity", "different parameters", "currently in progress", "previous request", "could not be found"} {
				if strings.Contains(h.audit.String(), message) {
					t.Errorf("audit records contain Shopify's message %q:\n%s", message, h.audit)
				}
			}
		})
	}
}

// A Caller can't move more units than it believes the source state holds.
func TestMovingMoreThanTheExpectedFromQuantityIsRefusedBeforeShopify(t *testing.T) {
	tests := map[string]struct {
		from, to     string
		quantity     int
		expectedFrom int
		want         string
	}{
		"out of available": {"available", "damaged", 51, 50, "quantity 51 is more than expected_from_quantity 50"},
		"out of damaged":   {"damaged", "available", 1, 0, "quantity 1 is more than expected_from_quantity 0"},
		"below zero":       {"available", "safety_stock", 1, -2, "quantity 1 is more than expected_from_quantity -2"},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h := newWriteHarness(t, grantedTo(inventoryMover))
			args := realMove()
			args["from"], args["to"], args["quantity"] = tt.from, tt.to, tt.quantity
			args["expected_from_quantity"] = tt.expectedFrom

			got := callToolError(t, h.connect(t), "move_inventory_quantity", args)
			for _, want := range []string{tt.want, "Nothing was changed"} {
				if !strings.Contains(got, want) {
					t.Errorf("error = %q, want it to say %q", got, want)
				}
			}
			if n := h.store.queries.Load(); n != 0 {
				t.Errorf("Shopify got %d queries, want none", n)
			}
		})
	}
}

func TestMovesOutsideTheAllowedPairsAreRefusedBeforeShopify(t *testing.T) {
	const notAState = "must be available, damaged, quality_control or safety_stock"
	tests := map[string]struct {
		from, to string
		want     string
	}{
		"between two non-available states": {"damaged", "safety_stock", "one of from and to must be available"},
		"back between them":                {"quality_control", "damaged", "one of from and to must be available"},
		"available to itself":              {"available", "available", "from and to are both available"},
		"a state to itself":                {"damaged", "damaged", "from and to are both damaged"},
		"from reserved":                    {"reserved", "available", `from ` + notAState + `, not "reserved"`},
		"to committed":                     {"available", "committed", `to ` + notAState + `, not "committed"`},
		"to incoming":                      {"available", "incoming", `to ` + notAState + `, not "incoming"`},
		"from on hand":                     {"on_hand", "available", `from ` + notAState + `, not "on_hand"`},
		"wrong case":                       {"Available", "damaged", `from ` + notAState + `, not "Available"`},
		"empty":                            {"available", "", `to ` + notAState + `, not ""`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h := newWriteHarness(t, grantedTo(inventoryMover))
			args := moveDryRun()
			args["from"], args["to"] = tt.from, tt.to

			got := callToolError(t, h.connect(t), "move_inventory_quantity", args)
			if !strings.Contains(got, tt.want) {
				t.Errorf("error = %q, want it to say %q", got, tt.want)
			}
			if n := h.store.queries.Load(); n != 0 {
				t.Errorf("Shopify got %d queries, want none", n)
			}
		})
	}
}

func TestMoveInputIsCheckedBeforeShopify(t *testing.T) {
	tests := map[string]struct {
		change func(args map[string]any)
		want   string
	}{
		"no units":        {func(a map[string]any) { a["quantity"] = 0 }, "quantity must be 1 or more, not 0"},
		"negative units":  {func(a map[string]any) { a["quantity"] = -2 }, "quantity must be 1 or more, not -2"},
		"no quantity":     {func(a map[string]any) { delete(a, "quantity") }, `missing properties: ["quantity"]`},
		"bad item ID":     {func(a map[string]any) { a["inventory_item_id"] = "123" }, "inventory_item_id must look like gid://shopify/InventoryItem/123"},
		"bad location ID": {func(a map[string]any) { a["location_id"] = testInventoryItem }, "location_id must look like gid://shopify/Location/123"},
		"no from":         {func(a map[string]any) { delete(a, "from") }, `missing properties: ["from"]`},
		"no to":           {func(a map[string]any) { delete(a, "to") }, `missing properties: ["to"]`},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			h := newWriteHarness(t, grantedTo(inventoryMover))
			args := moveDryRun()
			tt.change(args)

			got := callToolError(t, h.connect(t), "move_inventory_quantity", args)
			if !strings.Contains(got, tt.want) {
				t.Errorf("error = %q, want it to say %q", got, tt.want)
			}
			if n := h.store.queries.Load(); n != 0 {
				t.Errorf("Shopify got %d queries, want none", n)
			}
		})
	}
}
