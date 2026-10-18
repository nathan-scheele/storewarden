package shopify

// quotas are the Callers' Quotas: each Caller's share of the Store's
// query cost bucket.
type quotas struct {
	share float64 // each Quota's share of the Store's limits, in (0, 1]
}
