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
// at rate points a second, up to maximum. The limits are never hardcoded:
// they come from Shopify's responses.
type bucket struct {
	mu        sync.Mutex
	known     bool      // false until a response reports throttle status; until then the bucket counts as full
	available float64   // points in the bucket at time at
	maximum   float64   // maximumAvailable
	rate      float64   // restoreRate, points per second
	at        time.Time // when available was last set
}

// record updates the bucket from a response's cost report. The report
// replaces whatever the bucket has worked out since, including reservations
// for queries still in flight, because Shopify's figure is the true one.
func (b *bucket) record(now time.Time, cost *Cost) {
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
}

// take reserves cost points and returns how long to wait before sending the
// query. If deadline isn't zero and the wait would end after it, take
// reserves nothing and returns the wait with ok false.
func (b *bucket) take(now time.Time, cost float64, deadline time.Time) (wait time.Duration, ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.known {
		return 0, true
	}
	wait = b.waitLocked(now, cost)
	if !deadline.IsZero() && now.Add(wait).After(deadline) {
		return wait, false
	}
	// Spend the points now, so callers arriving meanwhile queue behind this
	// one. The bucket may go below zero: that is capacity already promised.
	b.available = b.currentLocked(now) - b.capLocked(cost)
	b.at = now
	return wait, true
}

// wait returns how long until the bucket holds cost points.
func (b *bucket) wait(now time.Time, cost float64) time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.known {
		return 0
	}
	return b.waitLocked(now, cost)
}

// currentLocked returns the points in the bucket at now, counting the refill.
func (b *bucket) currentLocked(now time.Time) float64 {
	refilled := b.available + b.rate*now.Sub(b.at).Seconds()
	return min(refilled, b.maximum)
}

// capLocked returns cost, but never more than a full bucket, so waiting for
// it always ends.
func (b *bucket) capLocked(cost float64) float64 {
	return min(cost, b.maximum)
}

// waitLocked returns how long until the bucket holds cost points, rounded
// up to the millisecond.
func (b *bucket) waitLocked(now time.Time, cost float64) time.Duration {
	short := b.capLocked(cost) - b.currentLocked(now)
	if short <= 0 || b.rate <= 0 {
		return 0
	}
	millis := math.Ceil(short / b.rate * 1000)
	return time.Duration(millis) * time.Millisecond
}

// waitForCapacity waits until the bucket holds cost points, or returns a
// *ThrottledError at once if that wait would outlast ctx's deadline.
func (c *Client) waitForCapacity(ctx context.Context, cost float64) error {
	deadline, _ := ctx.Deadline() // the zero time if ctx has no deadline
	wait, ok := c.bucket.take(c.clock.Now(), cost, deadline)
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
