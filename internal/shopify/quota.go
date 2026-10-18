package shopify

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// QuotaError means the Caller's Quota can't cover a query yet, so the query
// wasn't sent. It is worth retrying after Wait.
type QuotaError struct {
	Wait time.Duration // how long until the Quota should hold enough
}

func (e *QuotaError) Error() string {
	return fmt.Sprintf("shopify: the Caller's Quota is spent; retry in %s", e.Wait)
}

type quotaKey struct{}

// WithQuotaKey returns a copy of ctx in which Query charges every request
// it sends to Shopify to the Quota named key, in the same way as
// WithObserver. The key means nothing to the client: storewarden uses the
// Caller's node stable ID. A query whose ctx has no key has no Quota.
func WithQuotaKey(ctx context.Context, key string) context.Context {
	return context.WithValue(ctx, quotaKey{}, key)
}

// quotas are the Callers' Quotas. Each is a bucket like the Store's, whose
// maximum and restore rate are share times the Store's.
type quotas struct {
	share float64 // in (0, 1]

	mu sync.Mutex
	// byKey has a Quota for every key seen since storewarden started. None
	// is ever removed: there is one per tailnet node at most, and a
	// tailnet has few. It is kept only in memory, so a restart empties it.
	byKey map[string]*bucket
}

// get returns key's Quota at now, with its limits set from the Store's
// bucket, store. It returns nil until store knows Shopify's limits: until
// then the Quotas can't know theirs.
func (q *quotas) get(key string, now time.Time, store *bucket) *bucket {
	maximum, rate, known := store.limits()
	if !known {
		return nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	b, found := q.byKey[key]
	if !found {
		if q.byKey == nil {
			q.byKey = map[string]*bucket{}
		}
		// A new Quota starts full.
		b = &bucket{known: true, available: q.share * maximum, maximum: q.share * maximum, at: now}
		q.byKey[key] = b
	}
	// The Store's limits can change from one response to the next, for
	// example when the Store changes plan, and its Quotas follow them.
	b.setLimits(now, q.share*maximum, q.share*rate)
	return b
}

// chargeQuota charges cost to the Quota named by ctx's key, if it has one,
// or returns a *QuotaError at once if the Quota can't cover it. Until the
// Store's limits are known, it refuses nothing and charges nothing.
func (c *Client) chargeQuota(ctx context.Context, cost float64) error {
	key, ok := ctx.Value(quotaKey{}).(string)
	if !ok {
		return nil
	}
	now := c.clock.Now()
	b := c.quotas.get(key, now, &c.bucket)
	if b == nil {
		return nil
	}
	// With now as the deadline, take refuses any wait at all.
	if wait, ok := b.take(now, cost, now); !ok {
		return &QuotaError{Wait: wait}
	}
	return nil
}

// QuotaStatus is the state of one Caller's Quota, in Shopify's query cost
// points.
type QuotaStatus struct {
	Available   float64
	Maximum     float64
	RestoreRate float64 // points restored per second
}

// Quota returns the state of the Quota named by ctx's key (WithQuotaKey).
// ok is false if ctx has no key, or if Shopify hasn't yet reported the
// Store's limits, from which the Quota's come.
func (c *Client) Quota(ctx context.Context) (q QuotaStatus, ok bool) {
	key, ok := ctx.Value(quotaKey{}).(string)
	if !ok {
		return QuotaStatus{}, false
	}
	now := c.clock.Now()
	b := c.quotas.get(key, now, &c.bucket)
	if b == nil {
		return QuotaStatus{}, false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return QuotaStatus{Available: b.currentLocked(now), Maximum: b.maximum, RestoreRate: b.rate}, true
}
