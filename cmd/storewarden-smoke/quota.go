package main

// The "quota" and "quota-other" commands: one Caller runs out of its Quota
// while another keeps working. "quota" runs as the fully granted Caller and
// calls get_inventory until storewarden refuses it for its Quota.
// "quota-other" runs as the read-only Caller straight after, and makes the
// same call, which its own Quota still covers.

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/nathan-scheele/storewarden/internal/tools"
)

// quotaText is in the Tool error of a call refused for the Caller's Quota.
const quotaText = "your Quota of the Store's rate limit is used up"

// Calls made at once, and in all, while spending the Quota. With
// --quota-share 0.05 on the dev store, a Quota is 1000 points refilling at
// 50 a second, and get_inventory costs up to 30: several calls at once
// spend it in a few seconds. The cap stops a run that never sees a refusal.
const (
	quotaWorkers  = 4
	quotaMaxCalls = 400
)

// runQuota is the "quota ITEM" command.
func runQuota(ctx context.Context, s session, item string) scenario {
	sc := scenario{Steps: []step{}}
	var (
		mu      sync.Mutex
		calls   int
		refusal string // the first Quota refusal's Tool error
		failure string // the first other failure
	)
	var wg sync.WaitGroup
	for range quotaWorkers {
		wg.Go(func() {
			for {
				mu.Lock()
				if refusal != "" || failure != "" || calls >= quotaMaxCalls {
					mu.Unlock()
					return
				}
				calls++
				mu.Unlock()

				var inv tools.Inventory
				toolErr, err := s.call(ctx, "get_inventory", tools.GetInventoryInput{InventoryItemID: item}, &inv)
				mu.Lock()
				switch {
				case err != nil && failure == "":
					failure = err.Error()
				case strings.Contains(toolErr, quotaText) && refusal == "":
					refusal = toolErr
				case toolErr != "" && !strings.Contains(toolErr, quotaText) && failure == "":
					failure = "Tool error: " + toolErr
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()

	sc.record("calling get_inventory without pause runs out the Caller's Quota", refusal != "",
		fmt.Sprintf("refused after %d calls %s", calls, failure))
	sc.record("the refusal is retryable, names the Quota and suggests a wait",
		strings.HasPrefix(refusal, "retryable: ") && strings.Contains(refusal, "Retry the same call in "),
		refusal)
	return sc
}

// runQuotaOther is the "quota-other ITEM" command.
func runQuotaOther(ctx context.Context, s session, item string) scenario {
	sc := scenario{Steps: []step{}}
	var inv tools.Inventory
	const works = "another Caller's same call works while the first Caller's Quota is spent"
	if toolErr, err := s.call(ctx, "get_inventory", tools.GetInventoryInput{InventoryItemID: item}, &inv); !sc.failed(works, toolErr, err) {
		sc.record(works, len(inv.Levels) > 0, fmt.Sprintf("%d levels", len(inv.Levels)))
	}
	return sc
}
