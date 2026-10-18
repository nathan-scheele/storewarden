# storewarden

storewarden is an MCP server for one Shopify store. It joins your tailnet as its own node and offers a small, fixed set of Tools: four that read the store, one that corrects an inventory count, and one that moves units between Inventory states, such as from available to damaged. Grants in your tailnet policy file decide which Tools each device or user may call. Anything not granted is hidden and refused.

storewarden is an independent project. It is not made or supported by Tailscale or Shopify.

What it does:

- **Identity from the tailnet.** Every request is identified with Tailscale WhoIs. Callers need no Shopify credentials of their own.
- **Access from the policy file.** You grant Tools by exact name, in the same file as the rest of your tailnet access rules.
- **Guarded writes.** The two Write tools don't exist unless the operator turns on Write mode. Every call is a Dry run unless the Caller opts out. A real write needs an Idempotency key and an Expected quantity for each Inventory state it changes, and Shopify enforces both.
- **An Audit record for every attempted call.** A call that can't be audited doesn't happen.
- **Shopify's rate limit is respected.** storewarden learns the store's limits from Shopify's responses and waits for capacity.
- **Tailnet only.** It has no public listener.

Capitalised terms (Caller, Tool, Grant, Dry run…) are defined in [GLOSSARY.md](GLOSSARY.md). The full design is in [docs/design.md](docs/design.md), and the decisions behind it are in [docs/adr](docs/adr/):

- [ADR 0001](docs/adr/0001-stateless-mcp-for-per-request-identity.md): stateless MCP, so every request is identified and authorized.
- [ADR 0002](docs/adr/0002-write-tools-only-where-shopify-enforces-idempotency.md): Write tools only where Shopify enforces idempotency.
- [ADR 0003](docs/adr/0003-build-without-the-peer-api-server.md): build without tsnet's peer API server.

## Contents

- [Setup](#setup)
- [Example policy](#example-policy)
- [Tool reference](#tool-reference)
- [Threat model](#threat-model)
- [Audit records](#audit-records)
- [Operations](#operations)
- [Manual smoke test](#manual-smoke-test)
- [Development](#development)

## Setup

### 1. Go

You need Go 1.27.2 or later. The version comes from `go.mod`.

Every build and run needs the build tag `ts_omit_peerapiserver`. Without it, tsnet's peer API would listen on every host interface, so storewarden refuses to start ([ADR 0003](docs/adr/0003-build-without-the-peer-api-server.md)). The Makefile adds the tag for you:

```sh
make build          # writes bin/storewarden and bin/storewarden-smoke
```

Or build it yourself:

```sh
go build -tags ts_omit_peerapiserver -o bin/ ./cmd/storewarden
```

### 2. The Shopify store and app

storewarden talks to one store through a Shopify app that you own. `scripts/setup-shopify.sh` walks you through creating both, step by step:

```sh
scripts/setup-shopify.sh
```

It helps you:

1. sign in to the Shopify Dev Dashboard;
2. create a development store (with Shopify CLI if you have it);
3. create the app;
4. release an app version with exactly these Access scopes: `read_products`, `read_inventory`, `read_locations` and `write_inventory`;
5. install the app on the store;
6. copy the app's client ID and secret.

It writes `SHOPIFY_STORE`, `SHOPIFY_CLIENT_ID` and `SHOPIFY_CLIENT_SECRET` to `.env`, which is gitignored and created with mode 0600. Then it checks that Shopify issues a token with those scopes and that a test query works.

storewarden has no customer or order scopes, so even a bug in storewarden can't reach that data.

### 3. The tailnet tag and auth key

storewarden runs as a persistent, tagged node. Its default tag is `tag:storewarden`.

1. In your policy file, add `tag:storewarden` to `tagOwners` (see the [example policy](#example-policy)).
2. Create an auth key in the admin console, under Settings → Keys. Give it the tag `tag:storewarden`. Leave Ephemeral off, so the node keeps its name and Grants across restarts.
3. Pass it to storewarden as `TS_AUTHKEY`.

Instead of an auth key, you can give an OAuth client secret as `TS_CLIENT_SECRET`. tsnet uses it to create an auth key for the node's tags, so the OAuth client must be allowed to create keys with `tag:storewarden`.

tsnet reads these variables itself. storewarden has no Tailscale auth code of its own.

You need the key only on the first run. The node's state is saved in `--state-dir`, and later runs reuse it. With no key and no saved state, tsnet logs a login URL instead.

### 4. Flags and environment variables

Flags (from `storewarden -h`):

| Flag | Default | What it does |
|---|---|---|
| `--store` | (required) | The Store's domain, `<name>.myshopify.com`. |
| `--hostname` | `storewarden` | The tailnet node's name. |
| `--state-dir` | `storewarden` in your user config directory | Where tsnet keeps the node's state. On macOS that is `~/Library/Application Support/storewarden`; on Linux, `~/.config/storewarden`. |
| `--tags` | `tag:storewarden` | Comma-separated tags the node advertises. |
| `--allow-writes` | `false` | Write mode: serve the Write tools to Callers granted them. |
| `--quota-share` | `0.5` | Each Caller's Quota, as a share of the Store's rate limit: above 0 and at most 1. storewarden refuses to start with any other value. See [Rate limits](#rate-limits). |
| `--audit-log` | `-` | A file to append Audit records to, or `-` for stdout. |
| `--debug` | `false` | Log at debug level, including tsnet's backend logs. |

Environment variables:

| Variable | Read by | What it is |
|---|---|---|
| `SHOPIFY_CLIENT_ID` | storewarden | The Shopify app's client ID. Required. |
| `SHOPIFY_CLIENT_SECRET` | storewarden | The Shopify app's client secret. Required. |
| `TS_AUTHKEY` | tsnet | A Tailscale auth key, for the first run. |
| `TS_CLIENT_SECRET` | tsnet | A Tailscale OAuth client secret, instead of `TS_AUTHKEY`. |
| `TS_NO_LOGS_NO_SUPPORT` | tsnet | Set to `true` to stop tsnet uploading logs. See [Operations](#operations). |

storewarden reads the Shopify secret only from the environment. It never logs it. It gets a short-lived access token from Shopify with the client credentials grant, keeps it in memory, and renews it five minutes before it expires, or when Shopify refuses it.

To run it with the values the setup script saved:

```sh
set -a; . ./.env; set +a
TS_AUTHKEY=tskey-auth-... ./bin/storewarden --store "$SHOPIFY_STORE" --audit-log audit.jsonl
```

or, without building first:

```sh
go run -tags ts_omit_peerapiserver ./cmd/storewarden --store "$SHOPIFY_STORE"
```

Add `--allow-writes` to turn on Write mode. When the node is up, storewarden logs `serving MCP` with its tailnet name and addresses.

### 5. Connect an MCP client

storewarden speaks MCP over Streamable HTTP, at `/mcp` on port 80 of its tailnet address. Use plain `http://`:

```
http://storewarden.<your-tailnet>.ts.net/mcp
```

With MagicDNS, `http://storewarden/mcp` also works. Without it, use the node's tailnet IP address.

The MCP client must run on a device in the same tailnet, and a Grant must cover that device (see the next section). The client needs no token or header: storewarden knows who it is from the tailnet. For example, with Claude Code:

```sh
claude mcp add --transport http storewarden http://storewarden.<your-tailnet>.ts.net/mcp
```

Any MCP client that supports Streamable HTTP works the same way.

## Example policy

This is a complete policy file. It has two Grants:

- devices tagged `tag:agent` may use the four Read tools;
- members of `group:inventory` may read inventory, correct it, and move units between Inventory states.

```hujson
{
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
          {"tools": ["get_shop", "search_products", "get_product", "get_inventory"]},
        ],
      },
    },
    {
      "src": ["group:inventory"],
      "dst": ["tag:storewarden"],
      "ip":  ["tcp:80"],
      "app": {
        "github.com/nathan-scheele/storewarden": [
          {"tools": ["get_inventory", "set_inventory_quantity", "move_inventory_quantity"]},
        ],
      },
    },
  ],
}
```

If you already have a policy file, merge these parts into it rather than replacing it.

Each Grant has two parts, and a Caller needs both:

- `ip` lets the device reach storewarden on TCP port 80. Without it, packets never arrive.
- `app` names the Tools, under the capability `github.com/nathan-scheele/storewarden`. Its value is an object with one field, `tools`, a list of Tool names.

How storewarden reads the Grants:

- **Exact names.** Tool names must match exactly. There are no wildcards and no groups of Tools.
- **Union.** When several Grants match a Caller, the Caller gets every Tool any of them names. A Grant can only add Tools, never take them away. For example, if you add a third Grant that gives `group:inventory` the Read tools, Alice's devices can use all six Tools.
- **No capability, no access.** A device with network access but no storewarden capability gets HTTP 403 before any MCP exchange.
- **Typos deny.** A Grant value with an unknown field, such as `"tool"` instead of `"tools"`, denies the Caller and logs an error. A typo can't silently grant nothing.
- **Unknown Tools warn.** A Tool name storewarden doesn't have is logged as a warning and ignored. The Write tools, `set_inventory_quantity` and `move_inventory_quantity`, are Tools storewarden has even while Write mode is off, so naming them never warns. The rest of the Grant still applies.
- **Write mode wins.** The Write tools are allowed only when storewarden runs with `--allow-writes`. A Grant that names one is still valid while Write mode is off; it just allows nothing until Write mode is on, and it logs no warning.
- **Each Write tool by name.** A Grant for `set_inventory_quantity` doesn't cover `move_inventory_quantity`, or the other way round. You can let an agent record damaged stock without letting it set counts.

Tagged devices are full Callers. They are the usual way to run headless Callers, such as agents on servers. A tagged device has no user, so `group:inventory` matches only the untagged devices of its members.

Policy changes reach storewarden within seconds of saving them, with no restart, because it checks every request with WhoIs.

## Tool reference

Every Tool runs one fixed GraphQL document against Shopify's Admin API, version 2026-10. There is no Tool for raw queries.

| Tool | Kind | Access scopes |
|---|---|---|
| `get_shop` | Read | none |
| `search_products` | Read | `read_products` |
| `get_product` | Read | `read_products` |
| `get_inventory` | Read | `read_inventory`, `read_locations` |
| `set_inventory_quantity` | Write (only in Write mode) | `write_inventory`; its Dry run reads with `read_inventory` |
| `move_inventory_quantity` | Write (only in Write mode) | `write_inventory`; its Dry run reads with `read_inventory` |

Read tools are annotated read-only and idempotent. Write tools are annotated destructive and idempotent.

`tools/list` shows only the Tools the Caller is granted. A call to a Tool the Caller isn't granted fails with the same error as a Tool that doesn't exist: `unknown tool "name"`.

When a Tool fails, the Caller gets a Tool error whose text says what went wrong. IDs are checked before Shopify is called: a product ID must look like `gid://shopify/Product/123`, and so on.

### get_shop

Input: none.

Output:

| Field | Meaning |
|---|---|
| `name` | The store's name. |
| `myshopify_domain` | Its `<name>.myshopify.com` domain. |
| `plan` | Its Shopify plan. |
| `currency` | Its currency code. |
| `throttle_status` | Shopify's query cost bucket: `currently_available`, `maximum_available` and `restore_rate` (points per second). Omitted if Shopify didn't report it. |
| `quota` | The calling Caller's own Quota of that bucket, after this call: `available`, `maximum` and `restore_rate` (points per second). Omitted until Shopify has reported the bucket's limits. See [Rate limits](#rate-limits). |

### search_products

Input:

| Field | Meaning |
|---|---|
| `query` | Optional. Shopify's product search syntax, such as `title:snowboard` or `tag:winter status:active`. Empty matches every product. |
| `first` | Optional. Page size, 1 to 50. The default is 10. |
| `after` | Optional. The `next_cursor` from the previous page. |

Output: `products`, a list of `{id, title, handle, status, total_inventory}`, and `next_cursor`. `next_cursor` is absent on the last page.

### get_product

Input: `id`, a product ID such as `gid://shopify/Product/123`.

Output: `id`, `title`, `handle`, `status`, `tags`, and `variants`, a list of `{id, title, sku, inventory_item_id}`. It returns the first 100 variants. `sku` is omitted when empty. An unknown ID is a Tool error.

### get_inventory

Input: `inventory_item_id`, such as `gid://shopify/InventoryItem/123`, from `get_product`.

Output: `levels`, a list of `{location_id, location_name, available, on_hand, damaged, quality_control, safety_stock}`, one for each location that stocks the item, up to 100 locations. `available` is what can be sold. `on_hand` is what is physically there. `damaged`, `quality_control` and `safety_stock` are units on hand held out of sale in those Inventory states.

### set_inventory_quantity

Sets an inventory item's available quantity at one location. It exists only when storewarden runs with `--allow-writes`, and a Caller also needs a Grant that names it.

Input:

| Field | Meaning |
|---|---|
| `inventory_item_id` | The item, from `get_product`. |
| `location_id` | The location, such as `gid://shopify/Location/123`, from `get_inventory`. |
| `quantity` | The new available quantity, 0 or more. |
| `dry_run` | Optional. `true` (the default) reports the change without making it. `false` makes it. |
| `idempotency_key` | Required when `dry_run` is `false`. Up to 255 characters. Use a new one for each change, and the same one when retrying that change. A UUID is a good choice. |
| `expected_quantity` | Required when `dry_run` is `false`. The available quantity you believe is current, from a Dry run. |

Output:

| Field | Meaning |
|---|---|
| `dry_run` | Whether this was a Dry run. |
| `current` | The available quantity before the change. |
| `new` | The available quantity after the change. |
| `applied` | Whether the Store was changed. |
| `request_id` | This call's ID in the Audit records. |

#### Dry run, then execute

1. Find the item and location with `get_product` and `get_inventory`.
2. **Dry run.** Call with the item, location and new quantity, and leave out `dry_run`:

   ```json
   {"inventory_item_id": "gid://shopify/InventoryItem/123", "location_id": "gid://shopify/Location/456", "quantity": 49}
   ```

   storewarden reads the current available quantity and changes nothing:

   ```json
   {"dry_run": true, "current": 50, "new": 49, "applied": false, "request_id": "CK2CQH3USOJAX5PSIV4OS557U2"}
   ```

3. **Execute.** Call again with `dry_run: false`, a new Idempotency key, and the Dry run's `current` as the Expected quantity:

   ```json
   {"inventory_item_id": "gid://shopify/InventoryItem/123", "location_id": "gid://shopify/Location/456", "quantity": 49,
    "dry_run": false, "idempotency_key": "5a1d7e2b-8c3f-4a6e-9d0b-2c4e6f8a1b02", "expected_quantity": 50}
   ```

   ```json
   {"dry_run": false, "current": 50, "new": 49, "applied": true, "request_id": "4CHM6YONIBZJXAZ7OK7DIUXRUO"}
   ```

A real write calls Shopify's `inventorySetQuantities` with:

- the quantity name `available` and the reason `correction`;
- `changeFromQuantity` set to `expected_quantity`, so Shopify's compare-and-swap check always runs;
- Shopify's `@idempotent` directive with the Caller's `idempotency_key`;
- a reference document URI of `storewarden://audit/<request_id>`, so the change in Shopify's inventory history points back to its Audit records.

storewarden doesn't read the quantity before a real write. Shopify checks it. When the write succeeds, `current` is the Expected quantity, because Shopify confirmed it was current.

#### Errors from a real write

Shopify's refusals come back as plain Tool errors:

- **Stale quantity** (`CHANGE_FROM_QUANTITY_STALE`): the available quantity is no longer `expected_quantity`. Nothing changed. Run a new Dry run and use its `current` as `expected_quantity`, with a new key.
- **Key reused with different input** (`IDEMPOTENCY_KEY_PARAMETER_MISMATCH`): nothing changed. Use a new key for each new change.
- **Concurrent request** (`IDEMPOTENCY_CONCURRENT_REQUEST`): a call with this key is still in progress. The error starts with `retryable: ` and ends `Retry the same call in 2s`, like a rate-limited call. Retry exactly the same call, with the same key, after that wait.

#### What retries really do

The guarantee is this: a change is made at most once. Shopify's compare-and-swap check makes sure of that, even when the Idempotency key doesn't.

On the development store (Admin API 2026-10), retries behaved like this ([docs/design.md](docs/design.md), Write semantics):

- One retry with the same key and the same input returned the first call's result. storewarden reports that as `applied: true`, but the change was still made only once.
- Other retries, a few seconds and a minute after the first call, got the stale-quantity error instead.
- Identical calls sent at the same moment never got the concurrent-request error. One applied, and the rest got the stale-quantity error.

So if you retry a change that may already have gone through, expect either the original result or the stale-quantity error. After a stale-quantity error, run a Dry run: it shows whether the change was made.

### move_inventory_quantity

Moves units of an inventory item at one location between `available` and one of `damaged`, `quality_control` or `safety_stock`, so that Shopify records what happened to them instead of just a lower count. Like `set_inventory_quantity`, it exists only when storewarden runs with `--allow-writes`, and a Caller also needs a Grant that names it.

Input:

| Field | Meaning |
|---|---|
| `inventory_item_id` | The item, from `get_product`. |
| `location_id` | The location, from `get_inventory`. Both states are at this location. |
| `from` | The Inventory state to move units out of: `available`, `damaged`, `quality_control` or `safety_stock`. |
| `to` | The Inventory state to move them into, from the same list. Exactly one of `from` and `to` must be `available`. |
| `quantity` | How many units to move, 1 or more. |
| `dry_run` | Optional. `true` (the default) reports the move without making it. `false` makes it. |
| `idempotency_key` | Required when `dry_run` is `false`. As for `set_inventory_quantity`. |
| `expected_from_quantity` | Required when `dry_run` is `false`. The quantity you believe is current in `from`, from a Dry run. `quantity` can't be more than this. |
| `expected_to_quantity` | Required when `dry_run` is `false`. The quantity you believe is current in `to`, from a Dry run. |

A move between two non-available states, from a state to itself, or naming `reserved`, `committed`, `incoming` or `on_hand`, is refused before Shopify is called. Reserved and committed stock is held for orders, so it is out of reach.

Output:

| Field | Meaning |
|---|---|
| `dry_run` | Whether this was a Dry run. |
| `from` | `current` and `new`: the `from` state's quantity before and after the move. |
| `to` | `current` and `new`: the `to` state's quantity before and after the move. |
| `applied` | Whether the Store was changed. |
| `request_id` | This call's ID in the Audit records. |

For example, a stock check finds 3 damaged units:

1. **Dry run.**

   ```json
   {"inventory_item_id": "gid://shopify/InventoryItem/123", "location_id": "gid://shopify/Location/456", "from": "available", "to": "damaged", "quantity": 3}
   ```

   ```json
   {"dry_run": true, "from": {"current": 50, "new": 47}, "to": {"current": 0, "new": 3}, "applied": false, "request_id": "MJ2TJ7Q4XWFNEN6QLNEJ5YRGKY"}
   ```

2. **Execute**, with a new Idempotency key and both of the Dry run's `current` quantities:

   ```json
   {"inventory_item_id": "gid://shopify/InventoryItem/123", "location_id": "gid://shopify/Location/456", "from": "available", "to": "damaged", "quantity": 3,
    "dry_run": false, "idempotency_key": "60161b67-10f6-43bb-a353-111d474067ec", "expected_from_quantity": 50, "expected_to_quantity": 0}
   ```

   ```json
   {"dry_run": false, "from": {"current": 50, "new": 47}, "to": {"current": 0, "new": 3}, "applied": true, "request_id": "S3ZQ6WDUHJ4OAXG2TQ5PVY7L4E"}
   ```

To return them to sale later, move them back with `from` `damaged` and `to` `available`.

A real move calls Shopify's `inventoryMoveQuantities` with:

- `changeFromQuantity` set on both sides, to `expected_from_quantity` and `expected_to_quantity`, so Shopify's compare-and-swap check always runs for both states;
- Shopify's `@idempotent` directive with the Caller's `idempotency_key`;
- the non-available state's name as the reason (`damaged`, `quality_control` or `safety_stock`), so Shopify's inventory history says what happened;
- a reference document URI of `storewarden://audit/<request_id>`, which is also the ledger document URI on the non-available side. Shopify refuses a ledger document URI on `available`.

storewarden refuses a `quantity` above `expected_from_quantity` itself, because Shopify doesn't: on the development store, a move of 4 damaged units when 3 were there, with both compare-and-swap values right, was applied and left damaged at -1.

Shopify's refusals come back as plain Tool errors:

- **Stale quantity** (`CHANGE_FROM_QUANTITY_STALE`): a quantity changed since you read it. Nothing changed. Run a new Dry run and move with its `current` quantities, with a new key.
- **Key reused with different input** (`IDEMPOTENCY_KEY_PARAMETER_MISMATCH`): nothing changed. Use a new key for each new move.
- **Concurrent request** (`IDEMPOTENCY_CONCURRENT_REQUEST`): retryable, in the same form as for `set_inventory_quantity`.
- **Previous attempt failed** (`IDEMPOTENCY_PREVIOUS_ATTEMPT_FAILED`): an earlier call with this key failed, and Shopify won't use the key again. Nothing changed. Run a new Dry run and use a new key.

Any other refusal is passed through with Shopify's message, and recorded by its codes. As for `set_inventory_quantity`, the compare-and-swap checks make sure a move is made at most once, even when a retry's Idempotency key doesn't; see [What retries really do](#what-retries-really-do).

### Rate limits

Shopify limits each store with a bucket of query cost points. storewarden keeps one bucket per store and updates it from every response. It never hardcodes the limits.

Before each query, storewarden waits until the bucket should hold the most that query can cost. Each Tool's maximum cost is fixed and checked against a recording; `search_products` always counts its cost at the largest page size. Every Tool call has a 30-second deadline. If the wait would pass it, the call fails at once with a Tool error that starts with `retryable: ` and ends with how long to wait: `Retry the same call in <wait>`. If the Caller disconnects, the wait ends. If Shopify still throttles a query, storewarden waits and retries it once. A second throttle becomes the same retryable error.

Every Caller also has its own Quota: a share of the Store's bucket, set with `--quota-share` (default `0.5`, half). A Quota's maximum and refill rate are that share of the limits Shopify reports, so they follow the Store's plan. Until Shopify has reported them, Quotas refuse nothing. A Quota is kept per tailnet node, by its stable ID, so each device and each tagged node has its own. Quotas are kept in memory only and start full when storewarden starts.

Every query sent to Shopify for a Caller is charged to its Quota at that query's maximum cost: Read tools, Dry runs and real writes alike, and a throttled query's retry too. The Quota is checked first, and it never waits. A Caller whose Quota can't cover a query gets a Tool error at once, in the same form, naming its Quota: `retryable: your Quota of the Store's rate limit is used up, so nothing was done. Retry the same call in <wait>`. Other Callers carry on. `get_shop` shows a Caller its own Quota next to the Store's bucket, so it can pace itself.

## Threat model

### Assets

- The Store's data, and the accuracy of its inventory.
- The Shopify client secret and the access tokens made from it.
- The Audit trail.

### Root of trust

The root of trust is the tailnet policy file. Anyone who can edit it can grant themselves any Tool. storewarden can't defend against that, and doesn't try. Protect the policy file the way you protect the rest of your tailnet's access rules.

### Threats and mitigations

| Threat | Mitigation |
|---|---|
| A tailnet device with no Grant | WhoIs on every request. No storewarden capability means HTTP 403 before any MCP exchange, and a denial Audit record. |
| A granted Caller calls a Tool it isn't granted | `tools/list` hides it. `tools/call` answers exactly as for a Tool that doesn't exist, and writes a denial Audit record. |
| A granted Caller is manipulated through its inputs (prompt injection) | Fixed Tools only, with no raw queries. Write mode off by default. Dry run by default. Compare-and-swap on every write. Keep Grants small. |
| A write is retried or duplicated | The Idempotency key goes to Shopify's `@idempotent` directive, and compare-and-swap stops a second change. See [What retries really do](#what-retries-really-do). |
| A stale read leads to a wrong write | `expected_quantity`, and a move's `expected_from_quantity` and `expected_to_quantity`, become Shopify's `changeFromQuantity`, which is never null. |
| Another device reuses a Caller's MCP session | Stateless MCP. Every request is identified on its own ([ADR 0001](docs/adr/0001-stateless-mcp-for-per-request-identity.md)). |
| Public exposure | The only listener is tsnet's, inside the tailnet. No Funnel, serve config or host listener in the code, and `make never-public` fails the build if one appears. tsnet clears any saved serve config when the node starts. storewarden is built without tsnet's peer API server, which would otherwise listen on every host interface, and refuses to start if it isn't ([ADR 0003](docs/adr/0003-build-without-the-peer-api-server.md)). |
| Credential leak | Secrets come only from the environment and are never logged or written to Audit records. The access token is kept only in memory. The app has no customer or order scopes. |
| A busy, looping or manipulated Caller uses up the Store's rate limit | Every Caller has its own Quota, a share of the Store's rate limit (`--quota-share`). A Caller that has spent it gets a retryable error at once, and the others keep working. See [Rate limits](#rate-limits). |
| An action that isn't audited | Audit records fail closed: a call whose attempt record can't be written is refused. |
| Customer data in the audit trail | Shopify response bodies, and the messages in Shopify's errors, are never written to Audit records. Errors are recorded by their codes. |

### Accepted risks

- **Quotas are per node: a Caller that can add tailnet nodes gets one Quota per node.** Each node is a separate Caller with its own Quota. Enough nodes together can still use up the Store's rate limit. The Audit records name every node.
- **Someone with access to the host can change the audit log.** storewarden writes Audit records to a local file or stdout. It doesn't make them tamper-proof. Ship them off the host if you need that. Every real write and move is also in Shopify's inventory history, with a `storewarden://audit/<request_id>` reference to its Audit records, and no one on the host can change that.
- **A granted Writer can still set a wrong quantity, or move the wrong units.** The guards stop duplicate and stale writes and moves, and a move of more units than the Caller read, not a wrong number or wrong Inventory state from a Caller that read the current ones. The Audit records and Shopify's inventory history show who made each change.

### Why plain HTTP on the tailnet is fine

Tailnet traffic is already encrypted end to end by WireGuard, between the two devices' own keys. A Caller's tailnet address is tied to its node key, which is how WhoIs can name it. TLS on top would add a certificate to manage and nothing to the identity check. So storewarden serves plain HTTP, and only on its tailnet address.

### Why never to put `tailscale serve` or a proxy in front

storewarden decides who the Caller is from the address the request comes from. Behind `tailscale serve` or any other proxy, every request comes from the proxy. WhoIs then names the proxy, not the Caller. At best every request is refused. At worst every Caller gets the proxy's Grants, and the Audit records name the proxy for everything. Funnel would also make storewarden reachable from the internet. Callers must connect to storewarden's own tailnet address.

## Audit records

Audit records are JSON Lines, one record per line. They go to stdout by default, or to the file named by `--audit-log`, which is opened for appending with mode 0600. Operational logs go to stderr, so you can keep and ship the two separately.

### Which records are written

- **A denied request:** one `denied` record. That is a request refused with HTTP 403 (WhoIs failed, the Caller has no storewarden capability, or its Grant has an unknown field), or a call to a Tool the Caller isn't granted.
- **An allowed call:** an `attempt` record before Shopify is called, and a `result` record after. A call refused because the Caller's Quota is spent is an allowed call that failed: its `result` record's `error` names the Quota.

### Fail closed

- If the `attempt` record can't be written, the call is refused and Shopify is never called. The Caller gets the error `storewarden can't write its audit log, so the call was refused`.
- If any other record can't be written, storewarden logs an error. The call's result still goes back to the Caller, because the call has already happened.
- Any failed write turns `/healthz` unhealthy until storewarden restarts.

### Fields

| Field | Meaning |
|---|---|
| `time` | When the record was written, in UTC. |
| `request_id` | The request's ID. The `attempt` and `result` records of one call share it. It is also `request_id` in a Write tool's output and the end of its Shopify reference URI. |
| `phase` | `denied`, `attempt` or `result`. |
| `caller.node_id` | The node's stable ID. |
| `caller.node_name` | The node's tailnet name. |
| `caller.tags` | The node's tags, if it is tagged. |
| `caller.user` | The owner's login name, or `tagged` for a tagged node. |
| `tool` | The Tool called. Empty for a request refused before MCP. |
| `args` | The arguments exactly as the Caller sent them. |
| `decision` | `allow` or `deny`. |
| `reason` | Why a request was denied. |
| `dry_run` | For a Write tool, whether the call was a Dry run. `false` for Read tools. |
| `idempotency_key` | For a Write tool, the Idempotency key, if any. |
| `outcome` | On a `result` record, `ok` or `error`. |
| `error` | The error the Caller got, if any. A Shopify error is recorded by its codes only, such as `shopify: user errors, codes: INVALID_LOCATION`, because its message can quote Store data; the Caller still gets the message. |
| `shopify.requested_cost` | Shopify's estimated query cost, summed over every request the call sent, including a throttled request and its retry. |
| `shopify.actual_cost` | Shopify's actual query cost, summed the same way. A throttled request costs nothing. |
| `shopify.request_id` | Shopify's `X-Request-Id` for the call's last request. |
| `duration_ms` | On a `result` record, how long the call took. |

The `shopify` object appears only once Shopify has answered. Shopify response bodies, Shopify error messages and credentials are never recorded. Arguments are recorded as sent, so Callers shouldn't put secrets in them.

Names and tags are recorded for the audit trail only. storewarden never uses them to authorize.

### Example

The `result` record of a real write, from storewarden's tests:

```json
{"time":"2026-10-18T02:59:45.807395Z","request_id":"4CHM6YONIBZJXAZ7OK7DIUXRUO","phase":"result","caller":{"node_id":"nTESTAGENT","node_name":"agent.example.ts.net.","tags":["tag:agent"],"user":"tagged"},"tool":"set_inventory_quantity","args":{"dry_run":false,"expected_quantity":50,"idempotency_key":"5a1d7e2b-8c3f-4a6e-9d0b-2c4e6f8a1b02","inventory_item_id":"gid://shopify/InventoryItem/50260715896993","location_id":"gid://shopify/Location/85565997217","quantity":49},"decision":"allow","reason":"","dry_run":false,"idempotency_key":"5a1d7e2b-8c3f-4a6e-9d0b-2c4e6f8a1b02","outcome":"ok","error":"","shopify":{"requested_cost":10,"actual_cost":10,"request_id":"shopify-request-0001"},"duration_ms":0.782125}
```

## Operations

### Logs

Operational logs are JSON lines from Go's `log/slog`, on stderr. `--debug` turns on debug level, which adds tsnet's backend logs. Audit records are separate (see above).

### Health

`GET /healthz` returns `200 healthy`, or `503 unhealthy` once any Audit record has failed to write. It is the only path that skips WhoIs, and it says nothing else. It is still reachable only over the tailnet, from devices whose `ip` Grant reaches port 80.

### Shutdown

On SIGINT or SIGTERM, storewarden:

1. stops the HTTP server, giving in-flight requests up to 10 seconds to finish;
2. closes the tailnet node;
3. flushes the audit log file to disk and closes it.

### What tsnet uploads

By default, tsnet uploads its own logs to `log.tailscale.com`. Tailscale uses them for support and debugging. They are tsnet's backend logs: connection setup, network changes and client metrics. storewarden's own logs and Audit records are not uploaded. tsnet keeps its log ID in `tailscaled.log.conf` in the state directory.

To opt out, set this in storewarden's environment:

```sh
TS_NO_LOGS_NO_SUPPORT=true
```

tsnet then uploads nothing. As the name says, Tailscale support can't then see this node's logs. If your tailnet has network flow logs turned on, a node that opts out won't connect.

## Manual smoke test

The automated tests fake the tailnet and Shopify. Before a release, run storewarden for real against a development store.

Use a tailnet set aside for testing, not one with devices or rules you care about. The smoke test replaces the whole policy file.

`scripts/smoke-tailnet.sh` (or `make smoke`) is a step-by-step wizard. It needs `go`, `python3`, `lsof` and `mkfifo`, and the values from `scripts/setup-shopify.sh` in `.env`.

It builds storewarden and `cmd/storewarden-smoke` with `make build`, and starts storewarden in Write mode with its Audit records in a file. `storewarden-smoke` is a test Caller: a separate, ephemeral node that calls storewarden with the official MCP SDK client. The wizard runs two of them, tagged `tag:storewarden-smoke` and `tag:storewarden-smoke-ro`, and asks you for:

- an auth key for storewarden, tagged `tag:storewarden`, not ephemeral (not needed if the node is already registered);
- a reusable, ephemeral auth key for each test Caller, with its tag. They are saved in `.env` as `TS_SMOKE_AUTHKEY` and `TS_SMOKE_RO_AUTHKEY`.

It gives you each policy file to paste.

**Part 1, the tailnet and the Grants**, checks that:

- storewarden and the test Caller join the same tailnet;
- `/healthz` says healthy;
- with a Grant for `get_shop`, `tools/list` shows only `get_shop`, and `get_shop` returns the development store;
- storewarden has no TCP listener on the host;
- a Grant naming an unknown Tool logs a warning and still grants `get_shop`;
- a Grant with an unknown field gets HTTP 403 and an error log;
- a Grant with no storewarden capability gets HTTP 403, while `/healthz` still works.

**Part 2, every Tool and the Write tools**, grants one test Caller all six Tools and the other only the four Read tools. It checks that:

- the fully granted Caller sees all six Tools;
- `search_products` returns a page and its `next_cursor` gets the next one;
- `get_product` returns variants with inventory item IDs, and `get_inventory` returns quantities by location;
- for the first stocked inventory item:
  - a call with no `dry_run` is a Dry run and changes nothing;
  - executing with an `idempotency_key` and `expected_quantity` makes the change, and `get_inventory` shows it;
  - retrying the same call with the same key doesn't change the quantity again;
  - a new change with the old `expected_quantity` gets the stale-quantity error and changes nothing;
  - the quantity is put back;
- for the same item, moving 1 unit from available to damaged:
  - a move with no `dry_run` is a Dry run of both states and changes nothing;
  - executing with an `idempotency_key` and both Expected quantities makes the move;
  - retrying the same call with the same key doesn't move again;
  - a new move with the old Expected quantities gets the stale-quantity error and changes nothing;
  - a move of more than damaged holds is refused by storewarden, and, claiming damaged holds enough, what Shopify answers is recorded in the results;
  - the unit is moved back;
- the change and the move show in Shopify's inventory history with their `storewarden://audit/<request_id>` references, and the move with reason `damaged`. You check these in the Shopify admin; the wizard prints the link;
- the read-only Caller doesn't see either Write tool, and calling them gives `unknown tool "set_inventory_quantity"` and `unknown tool "move_inventory_quantity"`;
- SIGINT shuts storewarden down cleanly;
- the Audit records are JSON with every field in [Audit records](#audit-records), include attempt and result records for every Tool and the denials, show the real write's and the real move's Dry run, Idempotency key and Shopify request ID, the move's states and Expected quantities, and hold no Shopify response content.

It ends with a table of results, which it can post to the smoke-test issue. It leaves the storewarden node, its state, the policy and the keys in place for the next run, and says how to remove them.

It doesn't trigger Shopify's throttling. A live throttled response is recorded separately, in `cmd/storewarden/testdata/throttled.json`, and the client's tests use it.

## Development

```sh
make check   # gofmt, go vet, the never-public guard, staticcheck and go test -race
make ci      # check, plus govulncheck on go.mod's Go version
make hooks   # run make check before every commit, and the denylist check
```

The Makefile adds `-tags ts_omit_peerapiserver` wherever it's needed. If a gitignored `.public-denylist` file exists (one term per line), the hooks refuse a commit whose changes or message contain one of its terms. CI (`.github/workflows/ci.yml`) runs the same targets on every push and pull request. It never touches a real tailnet or Shopify, and it has no secrets.

The tests drive the whole server, with fakes for WhoIs, Shopify and the audit sink. The fake Shopify replays responses recorded from the development store with `scripts/record-fixture.sh`.

Reviewers judge changes against [CODING_STANDARDS.md](CODING_STANDARDS.md). Notes on the MCP SDK and tsnet behaviour the code relies on are in [docs/agents/dependency-notes.md](docs/agents/dependency-notes.md).

## License

BSD 3-Clause. See [LICENSE](LICENSE).
