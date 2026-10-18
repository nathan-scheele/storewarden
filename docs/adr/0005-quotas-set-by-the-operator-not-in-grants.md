# Quotas are set by the operator, not in Grants

Every Caller gets a Quota, a fixed share of the Store's rate limit, set by one flag on the instance. Quotas are kept per node stable ID, in memory only. Authorization otherwise lives in the tailnet policy file, so a reader might expect the Quota to be a field in the Grant. It isn't, for two reasons. Grant values from several Grants combine as a union, and a Quota would need its own rule for combining. And storewarden denies any Grant with a field it doesn't know, so a policy that sets a Quota would lock out Callers of every older instance that reads it.

## Considered Options

- **A `quota` field in the Grant, with the largest value winning.** Rejected for v2. It lets the policy give different Callers different Quotas, but it changes the Grant format and needs a combining rule. No user story needs different Quotas yet.
- **A Quota per user login.** Rejected. The Caller is the node, which is what WhoIs proves. A user with several devices gets one Quota per device, the same as tagged agent nodes.

## Consequences

- Quotas restart full whenever storewarden restarts, in keeping with v1's rule of no durable local storage.
- A Quota is a share of the limits learned from Shopify's responses, not a fixed number, so it adapts to the Store's plan.
- If a future need calls for a different Quota per Caller, the Grant field is the way to add it. Reopen this ADR first.
