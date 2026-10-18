# Write tools only where Shopify enforces idempotency

A Write tool is added only if the Shopify mutation behind it natively supports idempotency keys (`@idempotent`) and compare-and-swap (`changeFromQuantity`). For now that means inventory. The Caller's Idempotency key and Expected quantity go straight to Shopify. storewarden therefore keeps no local key store, its guarantees survive restarts, and a retried or stale write is rejected by the system that owns the data.

## Considered Options

- **Also expose tags or product updates, backed by an in-memory key store.** Rejected. Keys would be lost on restart, there would be two idempotency mechanisms to explain, and `productUpdate` has no compare-and-swap, so concurrent edits could overwrite each other.

## Consequences

Adding a Write tool for a mutation without native idempotency means reopening this decision and building a durable key store first.

## Note, 2026-10-17: what a retry gets back

Testing `set_inventory_quantity` against the dev store showed that Shopify's idempotency doesn't always return the first call's result. A retry with the same key and input returned the original result only once. Retries a few seconds and a minute later came back `CHANGE_FROM_QUANTITY_STALE`, and identical requests sent at once never got `IDEMPOTENCY_CONCURRENT_REQUEST`: one applied and the rest were stale. The compare-and-swap check blocked every second change, so the guarantee this decision relies on, that a retried write takes effect only once, held in every case.

The decision stands. What changes is what storewarden promises a Caller: a retry takes effect once, but may come back as the stale-quantity error rather than the original result. That error tells the Caller to run a Dry run, which shows whether the change was made. The details are in `docs/design.md` under Write semantics.
