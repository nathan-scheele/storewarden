package main

// The "move ITEM LOCATION" command: the move round trip in Part 2 of the
// smoke test. It runs as the Caller granted every Tool, on the inventory
// item and location the "tools" command picked. It moves 1 unit from
// available to damaged through a Dry run, a real move, a retry with the same
// Idempotency key and a move with stale Expected quantities, tries to move
// more than damaged holds, and finally moves the unit back.

import (
	"context"
	"fmt"
	"strings"

	"github.com/nathan-scheele/storewarden/internal/tools"
)

const moveTool = "move_inventory_quantity"

// Prefixes of the move Tool's errors the round trip expects.
const (
	staleMoveText = "a quantity changed since you read it"
	overMoveText  = "is more than expected_from_quantity"
)

// level returns the inventory level of item at location.
func (s session) level(ctx context.Context, item, location string) (tools.InventoryLevel, error) {
	var inv tools.Inventory
	toolErr, err := s.call(ctx, "get_inventory", tools.GetInventoryInput{InventoryItemID: item}, &inv)
	if err != nil {
		return tools.InventoryLevel{}, err
	}
	if toolErr != "" {
		return tools.InventoryLevel{}, fmt.Errorf("get_inventory: %s", toolErr)
	}
	for _, level := range inv.Levels {
		if level.LocationID == location {
			return level, nil
		}
	}
	return tools.InventoryLevel{}, fmt.Errorf("get_inventory shows no level at %s", location)
}

// states is what a check saw of the two Inventory states a move touches.
type states struct{ available, damaged int }

func (st states) String() string {
	return fmt.Sprintf("available=%d damaged=%d", st.available, st.damaged)
}

// states reads the available and damaged quantities of item at location.
func (s session) states(ctx context.Context, item, location string) (states, error) {
	level, err := s.level(ctx, item, location)
	return states{level.Available, level.Damaged}, err
}

// runMove is the "move" command.
func runMove(ctx context.Context, s session, item, location string) scenario {
	sc := scenario{Steps: []step{}, ItemID: item, LocationID: location}

	before, err := s.states(ctx, item, location)
	if err != nil {
		sc.Error = err.Error()
		return sc
	}
	sc.Before = before.available
	moved := states{before.available - 1, before.damaged + 1}
	toDamaged := tools.MoveInventoryQuantityInput{
		InventoryItemID: item, LocationID: location, From: "available", To: "damaged", Quantity: 1,
	}

	// A call with no dry_run is a Dry run.
	var dry tools.InventoryMove
	const dryRun = "a move without dry_run is a Dry run of both states and changes nothing"
	if toolErr, err := s.call(ctx, moveTool, toDamaged, &dry); sc.failed(dryRun, toolErr, err) {
		return sc
	}
	now, err := s.states(ctx, item, location)
	sc.record(dryRun, err == nil && dry.DryRun && !dry.Applied &&
		dry.From == tools.StateChange{Current: before.available, New: moved.available} &&
		dry.To == tools.StateChange{Current: before.damaged, New: moved.damaged} && now == before,
		fmt.Sprintf("from %+v to %+v, then %s %s", dry.From, dry.To, now, errText(err)))

	// The real move.
	execute := realMove(toDamaged, newKey(), dry.From.Current, dry.To.Current)
	var done tools.InventoryMove
	const executed = "executing with a key and both Expected quantities moves 1 unit to damaged"
	if toolErr, err := s.call(ctx, moveTool, execute, &done); sc.failed(executed, toolErr, err) {
		return sc
	}
	sc.ExecutedID = done.RequestID
	now, err = s.states(ctx, item, location)
	sc.record(executed, err == nil && done.Applied && !done.DryRun && now == moved,
		fmt.Sprintf("applied=%t, then %s %s; request_id %s", done.Applied, now, errText(err), done.RequestID))

	// The same call again, with the same key, must not move twice.
	var again tools.InventoryMove
	toolErr, err := s.call(ctx, moveTool, execute, &again)
	seen := "returned the original result"
	if toolErr != "" {
		seen = "Tool error: " + toolErr
	}
	now, statesErr := s.states(ctx, item, location)
	sc.record("retrying the move with the same key doesn't move again",
		err == nil && (toolErr == "" || strings.HasPrefix(toolErr, staleMoveText)) && statesErr == nil && now == moved,
		fmt.Sprintf("%s; then %s %s", seen, now, errText(err, statesErr)))

	// A new move based on the old read is refused.
	stale := realMove(toDamaged, newKey(), before.available, before.damaged)
	var refused tools.InventoryMove
	toolErr, err = s.call(ctx, moveTool, stale, &refused)
	now, statesErr = s.states(ctx, item, location)
	sc.record("a move with stale Expected quantities is refused with the stale-quantity error",
		err == nil && strings.HasPrefix(toolErr, staleMoveText) && statesErr == nil && now == moved,
		fmt.Sprintf("Tool error %q; then %s %s", toolErr, now, errText(err, statesErr)))

	// More than damaged holds. With honest Expected quantities storewarden
	// refuses it itself. With expected_from_quantity inflated to match, the
	// move reaches Shopify, and its answer is recorded.
	fromDamaged := tools.MoveInventoryQuantityInput{
		InventoryItemID: item, LocationID: location, From: "damaged", To: "available", Quantity: moved.damaged + 1,
	}
	honest := realMove(fromDamaged, newKey(), moved.damaged, moved.available)
	var over tools.InventoryMove
	honestErr, err := s.call(ctx, moveTool, honest, &over)
	inflated := realMove(fromDamaged, newKey(), fromDamaged.Quantity, moved.available)
	shopifyErr, inflatedErr := s.call(ctx, moveTool, inflated, &over)
	now, statesErr = s.states(ctx, item, location)
	// The check's name carries Shopify's answer, so that the results table
	// records it.
	answer := "an error"
	switch {
	case inflatedErr == nil && shopifyErr == "":
		answer = "the move was made"
	case strings.HasPrefix(shopifyErr, staleMoveText):
		answer = "the stale-quantity error"
	}
	sc.record("a move of more than damaged holds is refused by storewarden and changes nothing; claiming damaged holds enough, Shopify's answer is "+answer,
		err == nil && strings.Contains(honestErr, overMoveText) && inflatedErr == nil && shopifyErr != "" && statesErr == nil && now == moved,
		fmt.Sprintf("with honest expected quantities: Tool error %q; with expected_from_quantity %d, Shopify answered: %q; then %s %s",
			honestErr, inflated.Quantity, shopifyErr, now, errText(err, inflatedErr, statesErr)))

	// Move the unit back.
	back := realMove(tools.MoveInventoryQuantityInput{
		InventoryItemID: item, LocationID: location, From: "damaged", To: "available", Quantity: 1,
	}, newKey(), moved.damaged, moved.available)
	var restored tools.InventoryMove
	const movedBack = "moving the unit back to available restores both states"
	if toolErr, err := s.call(ctx, moveTool, back, &restored); sc.failed(movedBack, toolErr, err) {
		return sc
	}
	sc.RestoredID = restored.RequestID
	now, err = s.states(ctx, item, location)
	sc.record(movedBack, err == nil && restored.Applied && now == before,
		fmt.Sprintf("%s %s; request_id %s", now, errText(err), restored.RequestID))
	return sc
}

// realMove returns move as a real move with key, expecting from and to.
func realMove(move tools.MoveInventoryQuantityInput, key string, from, to int) tools.MoveInventoryQuantityInput {
	dryRun := false
	move.DryRun = &dryRun
	move.IdempotencyKey = key
	move.ExpectedFromQuantity = &from
	move.ExpectedToQuantity = &to
	return move
}
