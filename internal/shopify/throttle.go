package shopify

import (
	"context"
	"fmt"
	"math"
	"sync"
	"time"
)

// ThrottledError means the Store's query cost bucket couldn't serve a query
// in time: it wouldn't refill before the request's deadline, or Shopify
// throttled the query again after one retry. It is worth retrying after Wait.
type ThrottledError struct {
	Wait time.Duration // how long until the bucket should hold enough
}

func (e *ThrottledError) Error() string {
	return fmt.Sprintf("shopify: rate limited; retry in %s", e.Wait)
}

// bucket mirrors the Store's query cost bucket, so the client can wait for
// capacity instead of having Shopify throttle it. Shopify refills the bucket
// at rate points a second, up to maximum. Nothing here is hardcoded: the
// limits and each query's cost come from Shopify's responses.
type bucket struct {
	mu        sync.Mutex
	known     bool      // false until a response reports throttle status; until then the bucket counts as full
	available float64   // points in the bucket at time at
	maximum   float64   // maximumAvailable
	rate      float64   // restoreRate, points per second
	at        time.Time // when available was last set

	// costs holds the largest requestedQueryCost Shopify has reported for
	// each query document. That is Shopify's own estimate of the most the
	// query can cost, so it is what the client waits for. A query Shopify
	// hasn't costed yet is sent without waiting.
	costs map[string]float64
}

// record updates the bucket from a response's cost report. The report
// replaces whatever the bucket has worked out since, including reservations
// for queries still in flight, because Shopify's figure is the true one.
func (b *bucket) record(now time.Time, query string, cost *Cost) {
	if cost == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	t := cost.ThrottleStatus
	b.known = true
	b.available = t.CurrentlyAvailable
	b.maximum = t.MaximumAvailable
	b.rate = t.RestoreRate
	b.at = now
	if b.costs == nil {
		b.costs = map[string]float64{}
	}
	b.costs[query] = max(b.costs[query], cost.RequestedQueryCost)
}

// take reserves query's cost and returns how long to wait before sending
// it. If deadline isn't zero and the wait would end after it, take reserves
// nothing and returns the wait with ok false.
func (b *bucket) take(now time.Time, query string, deadline time.Time) (wait time.Duration, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.known {
		return 0, true
	}
	wait = b.waitLocked(now, query)
	if !deadline.IsZero() && now.Add(wait).After(deadline) {
		return wait, false
	}
	// Spend the points now, so callers arriving meanwhile queue behind this
	// one. The bucket may go below zero: that is capacity already promised.
	b.available = b.currentLocked(now) - b.costLocked(query)
	b.at = now
	return wait, true
}

// wait returns how long until the bucket holds enough for query.
func (b *bucket) wait(now time.Time, query string) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.known {
		return 0
	}
	return b.waitLocked(now, query)
}

// currentLocked returns the points in the bucket at now, counting the refill.
func (b *bucket) currentLocked(now time.Time) float64 {
	refilled := b.available + b.rate*now.Sub(b.at).Seconds()
	return min(refilled, b.maximum)
}

// costLocked returns what query is expected to cost. It is never more than
// a full bucket, so waiting for it always ends.
func (b *bucket) costLocked(query string) float64 {
	return min(b.costs[query], b.maximum)
}

// waitLocked returns how long until the bucket holds enough for query,
// rounded up to the millisecond.
func (b *bucket) waitLocked(now time.Time, query string) time.Duration {
	short := b.costLocked(query) - b.currentLocked(now)
	if short <= 0 || b.rate <= 0 {
		return 0
	}
	millis := math.Ceil(short / b.rate * 1000)
	return time.Duration(millis) * time.Millisecond
}

// waitForCapacity waits until the bucket can serve query, or returns a
// *ThrottledError at once if that wait would outlast ctx's deadline.
func (c *Client) waitForCapacity(ctx context.Context, query string) error {
	deadline, _ := ctx.Deadline() // the zero time if ctx has no deadline
	wait, ok := c.bucket.take(c.clock.Now(), query, deadline)
	if !ok {
		return &ThrottledError{Wait: wait}
	}
	if wait == 0 {
		return nil
	}
	select {
	case <-c.clock.After(wait):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
