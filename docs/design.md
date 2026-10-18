# storewarden v1 design

storewarden is a single Go binary. It joins a tailnet with `tsnet` and serves a small, fixed set of Shopify Admin operations as MCP Tools. Every request is identified with Tailscale WhoIs. Grants in the tailnet policy file decide which Tools each Caller may use, and anything not granted is denied. Capitalised terms (Caller, Tool, Grant…) are defined in [GLOSSARY.md](../GLOSSARY.md). The non-obvious decisions are in [docs/adr](adr/).

## Goals

- Callers on the tailnet can read the Store and correct inventory, and nothing else.
- Authorization lives in the tailnet policy file, not in storewarden's config.
- Write tools are off by default, dry run by default, idempotent and compare-and-swap.
- Every attempted Tool call leaves an Audit record.
- Shopify's cost-based rate limit is respected.
- Reachable only over the tailnet. Never public.

## Non-goals (v1)

- A raw GraphQL Tool, or any Tool not listed below.
- More than one Store per process. Run a second instance instead.
- Per-Caller rate quotas.
- MCP sampling, elicitation, resources or prompts (see [ADR 0001](adr/0001-stateless-mcp-for-per-request-identity.md)).
- HTTPS on the tailnet (WireGuard already encrypts tailnet traffic).
- Running behind `tailscale serve` or any proxy. That would break WhoIs.

## Dependencies

| Module | Version | Why |
|---|---|---|
| Go | 1.27.2+ | tailscale.com v1.104 needs 1.27.1; 1.27.2 has the net/http security fixes govulncheck requires |
| `tailscale.com` | v1.104.1 | `tsnet`, `client/local`, `tailcfg` |
| `github.com/modelcontextprotocol/go-sdk` | v1.8.0 | Official MCP SDK, v1 compatibility promise |

Nothing else outside the standard library: `log/slog`, `net/http`, `encoding/json`, `crypto/rand`, `os/signal`.

## Layout

```
cmd/storewarden/main.go   flags, tsnet, wiring, graceful shutdown
cmd/storewarden/handler.go  the assembled HTTP handler (the primary test seam)
cmd/storewarden-smoke     test Caller for the manual smoke test (scripts/smoke-tailnet.sh)
internal/authz            WhoIs → Caller + allowed Tools; HTTP + MCP middleware
internal/audit            Audit record writer (JSON Lines, fail closed)
internal/shopify          GraphQL client, token source, cost throttle
internal/tools            the five Tool definitions
```

## Request flow

```
tailnet peer ──HTTP POST /mcp──▶ tsnet listener :80
  → authz.HTTP     WhoIs(r.RemoteAddr). No capability → 403 + Audit record.
                   Caller + allowed set + request ID go into ctx.
  → MCP handler    go-sdk Streamable HTTP, Stateless: true
  → authz.MCP      tools/list: drop Tools not allowed.
                   tools/call: Tool not allowed → "unknown tool" + Audit record.
  → tool handler   attempt Audit record (fail closed) → shopify client → result Audit record
```

Stateless mode means WhoIs and authorization run on every POST, not once per session ([ADR 0001](adr/0001-stateless-mcp-for-per-request-identity.md)). `authz.MCP` checks every `tools/call` even though `tools/list` is already filtered.

## Identity and authorization

**Caller** comes from `local.Client.WhoIs(ctx, r.RemoteAddr)`:

- Always: `Node.StableID`, `Node.Name`.
- Tagged (`Node.IsTagged()`): `Node.Tags`, and user is recorded as `"tagged"`. Tagged nodes are allowed; they are the main case for software Callers.
- Untagged: `UserProfile.LoginName`.

Names and tags are recorded for the audit trail only and never used to authorize.

**Capability** `github.com/nathan-scheele/storewarden`. Its value is:

```json
{ "tools": ["get_shop", "set_inventory_quantity"] }
```

- Read with `tailcfg.UnmarshalCapJSON`. Values from every matching Grant arrive as one flat list. The allowed set is the **union** of all `tools`.
- Unknown JSON fields cause an error, so the Caller is denied and an error is logged. A typo shouldn't silently grant nothing.
- A tool name in a Grant that storewarden doesn't have is logged as a warning and ignored. A Write tool counts as one storewarden has even when Write mode is off, so a policy that grants it doesn't warn; the Grant just allows nothing until Write mode is on.
- WhoIs fails, or no capability at all → HTTP 403.

### Example policy

`ip` grants network reach to port 80. `app` grants the Tools.

```hujson
"groups": {
  "group:inventory": ["alice@example.com"],
},
"tagOwners": {
  "tag:storewarden": ["autogroup:admin"],
  "tag:agent":       ["autogroup:admin"],
},
"grants": [
  {
    "src": ["tag:agent"],
    "dst": ["tag:storewarden"],
    "ip":  ["tcp:80"],
    "app": {
      "github.com/nathan-scheele/storewarden": [
        {"tools": ["get_shop", "search_products", "get_product", "get_inventory"]}
      ]
    }
  },
  {
    "src": ["group:inventory"],
    "dst": ["tag:storewarden"],
    "ip":  ["tcp:80"],
    "app": {
      "github.com/nathan-scheele/storewarden": [
        {"tools": ["get_inventory", "set_inventory_quantity"]}
      ]
    }
  }
]
```

## Tools

All Tools use fixed GraphQL documents against Admin API **2026-10**, pinned as a constant. Each Tool's maximum query cost is known and checked by a test.

| Tool | Kind | Shopify | Access scope |
|---|---|---|---|
| `get_shop` | Read | `shop` | none |
| `search_products` | Read | `products(query:, first:, after:)` | `read_products` |
| `get_product` | Read | `product(id:)` + variants (first 100) | `read_products` |
| `get_inventory` | Read | `inventoryItem(id:).inventoryLevels` with `quantities(names: ["available","on_hand","damaged","quality_control","safety_stock"])` | `read_inventory`, `read_locations` |
| `set_inventory_quantity` | Write | `inventorySetQuantities @idempotent` | `write_inventory` |
| `move_inventory_quantity` | Write | `inventoryMoveQuantities @idempotent` | `write_inventory` |

**Inputs and outputs** (Go structs, schema inferred by the SDK):

- `get_shop` → name, myshopify domain, plan, currency, and the current throttle status.
- `search_products` (`query` string in Shopify search syntax; `first` 1–50, default 10; optional `after` cursor) → `products[{id, title, handle, status, total_inventory}]`, `next_cursor`.
- `get_product` (`id` as a product GID) → id, title, handle, status, tags, `variants[{id, title, sku, inventory_item_id}]`. Each variant's `title` and `sku` are there so a Caller can tell variants apart before it reads their inventory.
- `get_inventory` (`inventory_item_id`) → `levels[{location_id, location_name, available, on_hand, damaged, quality_control, safety_stock}]`.
- `set_inventory_quantity`. Input: `inventory_item_id`, `location_id`, `quantity` (≥ 0), and these three:
  - `dry_run`: default **true**.
  - `idempotency_key`: required when `dry_run` is false; 1–255 characters, UUID recommended.
  - `expected_quantity`: required when `dry_run` is false.

  Output: `dry_run`, `current`, `new`, `applied`, `request_id`.
- `move_inventory_quantity`. Input: `inventory_item_id`, `location_id`, `from` and `to` (each one of `available`, `damaged`, `quality_control`, `safety_stock`, exactly one of them `available`), `quantity` (≥ 1), `dry_run` (default **true**), and, required when `dry_run` is false, `idempotency_key`, `expected_from_quantity` and `expected_to_quantity`.

  Output: `dry_run`, `from` and `to` (each `{current, new}`), `applied`, `request_id`.

Annotations: Read tools get `ReadOnlyHint` and `IdempotentHint`. Write tools get `DestructiveHint` and `IdempotentHint`.

### Write semantics

Write tools are guarded by two keys. They are only registered when the server starts with `--allow-writes` (**Write mode**). Even then, a Caller also needs a Grant that names the Tool.

`set_inventory_quantity`:

1. **Dry run** (default): read the current `available` quantity at the location and return `current → new`. Nothing is written.
2. **Execute** (`dry_run: false`): call `inventorySetQuantities` with:
   - `name: "available"`
   - `reason: "correction"`
   - `referenceDocumentUri: "storewarden://audit/<request-id>"`, so Shopify's inventory history links back to the Audit record
   - `changeFromQuantity: expected_quantity`, which is never null, so the compare-and-swap check always runs
   - `@idempotent(key: idempotency_key)`

   Shopify enforces both guarantees ([ADR 0002](adr/0002-write-tools-only-where-shopify-enforces-idempotency.md)). These errors are passed back to the Caller in plain words:
   - `CHANGE_FROM_QUANTITY_STALE` → "quantity changed since you read it; dry-run again".
   - `IDEMPOTENCY_KEY_PARAMETER_MISMATCH` → "key reused with different input".
   - `IDEMPOTENCY_CONCURRENT_REQUEST` → retryable: the Tool error starts with `retryable: ` and ends `Retry the same call in 2s`, the same form as a rate-limited call.

   Retryable errors are marked in their text, not as structured content. A Tool error reaches the Caller as text with `isError: true`; `structuredContent` would have to match the Tool's output schema, which describes a successful result, and an LLM Caller reads the text either way.

   Seen on the dev store (2026-10):
   - The input type takes `changeFromQuantity`; it has no `compareQuantity`.
   - A retry with the same key and input once returned the first call's result. Other retries, a few seconds and a minute after the first call, came back `CHANGE_FROM_QUANTITY_STALE` instead. So a Caller retrying a change that went through may get the stale-quantity error rather than the original result; the error tells it to dry-run, which shows whether the change was made.
   - Identical requests with the same key sent at once never got `IDEMPOTENCY_CONCURRENT_REQUEST`: one applied and the rest got `CHANGE_FROM_QUANTITY_STALE`. That error's test response is therefore built from Shopify's documented code and message, not recorded.
   - In every case the compare-and-swap check stopped a second change.

`move_inventory_quantity` is guarded the same way, by Write mode and its own Grant; a Grant for `set_inventory_quantity` doesn't cover it. It meets [ADR 0002](adr/0002-write-tools-only-where-shopify-enforces-idempotency.md) as it stands (see its 2026-10-18 note) and needs no scope beyond `write_inventory`.

1. **Checks before Shopify:** exactly one of `from` and `to` is `available`, and they differ. `reserved`, `committed`, `incoming` and `on_hand` are never accepted. A real move also needs the Idempotency key and both Expected quantities, and `quantity` may not exceed `expected_from_quantity`. Anything else is refused in plain words.
2. **Dry run** (default): read `available`, `damaged`, `quality_control` and `safety_stock` at the location with one fixed query, and return both states' `current → new`. Nothing is written.
3. **Execute** (`dry_run: false`): call `inventoryMoveQuantities` with one change:
   - `changeFromQuantity` on `from` and on `to`, from `expected_from_quantity` and `expected_to_quantity`. Both are `Int!` in the document, so neither is ever null.
   - `reason`: the non-available state's name (`damaged`, `quality_control` or `safety_stock`).
   - `referenceDocumentUri: "storewarden://audit/<request-id>"`, which is also the `ledgerDocumentUri` on the non-available side. The document takes a nullable ledger URI for each side and the Tool sets exactly one: Shopify requires one except on `available`, and refuses one there.
   - `@idempotent(key: idempotency_key)`

   Errors in plain words, sharing `set_inventory_quantity`'s translation:
   - `CHANGE_FROM_QUANTITY_STALE` (reported for either state or both) → "a quantity changed since you read it; dry-run again".
   - `IDEMPOTENCY_KEY_PARAMETER_MISMATCH` → "key reused with different input".
   - `IDEMPOTENCY_CONCURRENT_REQUEST` → retryable, as for set.
   - `IDEMPOTENCY_PREVIOUS_ATTEMPT_FAILED` → "an earlier call with this key failed; dry-run again and use a new key". Only the move Tool translates it; for set it is passed through like any other user error.
   - Any other user error is passed through, and recorded by its codes.

   Seen on the dev store (2026-10):
   - An explicit null `ledgerDocumentUri` on the `available` side is accepted, in either direction.
   - A move of more than the source state holds, with both `changeFromQuantity` values right, was applied and left the source negative (3 damaged, 4 moved, -1 left). Shopify doesn't stop it, so storewarden's own `quantity ≤ expected_from_quantity` check does.
   - Resending a move that failed with a user error, with the same key and input, got the same user error, not `IDEMPOTENCY_PREVIOUS_ATTEMPT_FAILED`. That error's and the concurrent-request error's test responses are therefore built from Shopify's documented codes and messages.

## Shopify client (`internal/shopify`)

- **Endpoint:** `POST https://<store>.myshopify.com/admin/api/2026-10/graphql.json`, header `X-Shopify-Access-Token`.
- **Token source:** client credentials grant (`POST /admin/oauth/access_token`, `grant_type=client_credentials`). The token is cached in memory and reissued 5 minutes before `expires_in` (24h) runs out, and once on a 401. If `expires_in` is missing or zero, the token is taken to last 24 hours and a warning is logged. A mutex makes sure only one reissue runs at a time. Tokens are never logged.
- **Throttle:** one bucket per Store.
  - After every response, record `extensions.cost.throttleStatus` (`currentlyAvailable`, `maximumAvailable`, `restoreRate`). Limits are never hardcoded; until the first response arrives, the bucket assumes it's full.
  - Before a call, estimate the refill needed for the query's known maximum cost. Each Tool passes that cost, a constant checked against a recording (`internal/tools/cost_test.go`), so the throttle can decide from the very first call. `search_products` passes its cost at the largest page size, 50, whatever page size the Caller asks for. A fixed maximum is always safe, because Shopify works out a query's requested cost from its text and variables before running it ([rate limits](https://shopify.dev/docs/apps/build/apis/graphql-admin/rate-limits)). If the wait ends before the request's deadline, wait. If not, fail fast with a retryable error (`shopify.ThrottledError`) that includes the suggested wait. Every Tool call gets a 30-second deadline (`callTimeout` in `internal/tools`), so this can happen; the Caller sees a Tool error starting `retryable: ` that ends `Retry the same call in <wait>`.
  - On `THROTTLED`, retry once after the calculated wait. Errors and `userErrors` come back as typed Go errors.
- Shopify's `X-Request-Id` response header goes into the Audit record.

## Audit (`internal/audit`)

- JSON Lines written to `--audit-log` (path, or `-` for stdout; the default is `-`). This is separate from the operational logs, which go to stderr.
- **Records:** one per denial. For an allowed call, an `attempt` record before Shopify is called and a `result` record after. A request refused with HTTP 403 is refused before any MCP exchange, so its denial record has an empty `tool` and no `args`; a denied `tools/call` records both.
- **Fail closed:** if the `attempt` record can't be written, the call is refused. Any record that can't be written, whether a denial, an attempt or a result, is logged as an error and turns `/healthz` unhealthy until restart: the audit trail is broken either way.
- **Fields:**
  - time, `request_id`, `phase` (`denied`, `attempt` or `result`)
  - caller: `node_id`, `node_name`, `tags`, `user`
  - `tool`, `args`, `decision`, `reason`
  - `dry_run`, `idempotency_key`
  - `outcome`, `error`. A Shopify error (GraphQL errors or `userErrors`) is recorded as storewarden's wording plus Shopify's error codes, without Shopify's messages, which can quote Store data. The Caller's Tool error keeps the messages.
  - shopify: `requested_cost`, `actual_cost`, `request_id`. Costs are summed over every response Shopify sent for the call, so a throttled request and its retry both count; `request_id` is the last response's.
  - `duration_ms`
- **Never logged:** Shopify response bodies and error messages (they can contain customer data) and credentials.

## Process

**Flags:**

| Flag | Default | Purpose |
|---|---|---|
| `--store` | (required) | `<name>.myshopify.com` |
| `--hostname` | `storewarden` | tailnet node name |
| `--state-dir` | `$UserConfigDir/storewarden` | tsnet state; persistent, not ephemeral |
| `--tags` | `tag:storewarden` | passed to `AdvertiseTags` |
| `--allow-writes` | `false` | Write mode |
| `--audit-log` | `-` | Audit record destination |
| `--debug` | `false` | debug-level logs, including tsnet's `Logf` |

**Environment:**

- `SHOPIFY_CLIENT_ID`, `SHOPIFY_CLIENT_SECRET`.
- `TS_AUTHKEY` or `TS_CLIENT_SECRET`. tsnet reads these itself; storewarden adds no Tailscale auth code.

**Listening:** only `tsnet.Server.Listen("tcp", ":80")`, serving `/mcp` and `/healthz`. `/healthz` is unauthenticated, returns 200 or 503 with no body detail, and is the only unauthenticated path. storewarden never calls `ListenFunnel` or `SetServeConfig` and never opens a host listener. `make never-public` (`scripts/never-public`), run by CI and the pre-commit hook, fails if non-test code calls either of those or a standard-library host listener (including an `httptest` server, which listens on the host's loopback), or uses tsnet without checking `buildfeatures.HasPeerAPIServer`. It is built with `-tags ts_omit_peerapiserver`, because tsnet's peer API would otherwise listen on every host interface, and it refuses to start without that tag ([ADR 0003](adr/0003-build-without-the-peer-api-server.md)).

**Logging:** `log/slog` JSON handler on stderr. tsnet's backend logs go to slog at debug level. tsnet still uploads its logs to log.tailscale.com, as tsnet does by default. The README explains this and how to opt out (`TS_NO_LOGS_NO_SUPPORT=true`).

**Shutdown:** `signal.NotifyContext` for SIGINT and SIGTERM → `http.Server.Shutdown` with a 10 s timeout → `tsnet.Server.Close` → flush and close the audit log. `http.Server` sets `ReadHeaderTimeout`.

## Threat model (README outline)

- **Assets:** Store data, inventory accuracy, the Shopify client secret and token, and the Audit trail.
- **Root of trust:** the tailnet policy file. Anyone who can edit it can grant themselves any Tool. This is out of scope and stated plainly.

| Threat | Mitigation |
|---|---|
| Ungranted tailnet peer | WhoIs on every request; no capability → 403; Tools hidden and denied |
| Granted but prompt-injected Caller | Fixed Tools only (no raw query), Read tools by default, Write mode off, dry run by default, compare-and-swap, small grant sets |
| Retried or duplicated write | Shopify-enforced Idempotency key |
| Stale read leading to a wrong write | Expected quantity → `changeFromQuantity`, on both sides of a move |
| Session reused by another peer | Stateless MCP (ADR 0001) |
| Public exposure | No Funnel, serve or host listener code path; CI guard; tsnet clears stale serve config at startup |
| Credential leak | Secrets only from env; never logged; token kept only in memory |
| Unaudited action | Audit fails closed |
| Granted Caller draining the cost bucket | Accepted v1 risk; per-Caller quotas are future work |
| Audit log tampering on the host | Out of scope; ship logs off-host. Every real write and move is also referenced in Shopify's inventory history |

## Testing and CI

Two test seams:

- **Assembled server handler (primary).** The real middleware, MCP handler and Tools, with three faked edges:
  - WhoIs: a one-method `WhoIser` interface.
  - Shopify: an `httptest` server.
  - The audit sink: an `io.Writer` that can be made to fail.

  Driven by the SDK client (`StreamableClientTransport`) over `httptest`. Covers authorization, list filtering, Write mode, dry run vs execute, Audit records and fail closed.
- **Shopify client.** An `httptest` server plus an injected clock. Covers token reissue, the throttle's wait-vs-fail-fast decision, the `THROTTLED` retry and typed errors.
- GitHub Actions (`.github/workflows/ci.yml`): `go vet`, `staticcheck`, `go test -race ./...`, `govulncheck`, plus the never-public check. Each step is a make target; `make ci` runs them all locally.
- Manual: `make smoke`, a documented run against a dev store.

## Weekend build order

1. Module, `main.go` with tsnet, `/healthz` and graceful shutdown. Connect from another tailnet device.
2. `internal/shopify`: token source, client, throttle, with tests.
3. `internal/authz`: WhoIs middleware and the capability parsing, with tests.
4. `internal/tools`: the four Read tools wired into the MCP handler, plus the end-to-end tests.
5. `internal/audit`, wired in fail-closed.
6. `set_inventory_quantity` with dry run, compare-and-swap and idempotency.
7. CI workflow, README (setup, example policy, threat model), LICENSE (BSD-3-Clause), smoke test against the dev store.
