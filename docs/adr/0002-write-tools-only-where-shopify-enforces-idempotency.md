# Write tools only where Shopify enforces idempotency

A Write tool is added only if the Shopify mutation behind it natively supports idempotency keys (`@idempotent`) and compare-and-swap (`changeFromQuantity`). For now that means inventory. The Caller's Idempotency key and Expected quantity go straight to Shopify. storewarden therefore keeps no local key store, its guarantees survive restarts, and a retried or stale write is rejected by the system that owns the data.

## Considered Options

- **Also expose tags or product updates, backed by an in-memory key store.** Rejected. Keys would be lost on restart, there would be two idempotency mechanisms to explain, and `productUpdate` has no compare-and-swap, so concurrent edits could overwrite each other.

## Consequences

Adding a Write tool for a mutation without native idempotency means reopening this decision and building a durable key store first.
