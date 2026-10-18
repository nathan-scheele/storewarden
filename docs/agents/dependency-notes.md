# Dependency behaviour storewarden relies on

Facts about the MCP SDK (v1.8.0), tsnet (tailscale.com v1.104.1) and Shopify's Admin API that shape storewarden's code and can't be seen from it. The SDK and tsnet facts were checked against the module source in `~/go/pkg/mod`, the Shopify ones against the dev store; re-check when a version changes.

## MCP SDK (`github.com/modelcontextprotocol/go-sdk/mcp`)

- **Context reaches the middleware.** In stateless mode the handler connects each request's session with `req.Context()`, so values the HTTP middleware puts in the context (the Caller) are visible to receiving middleware and tool handlers. `streamable.go`, `serveStateless`.
- **Unknown Tool error.** A missing Tool returns `&jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: fmt.Sprintf("unknown tool %q", name)}`. `authz.MCP` copies it exactly so a denied call looks the same. `server.go`, `callTool`.
- **List results are cacheable, public by default.** `ListToolsResult` embeds `Cacheable`; `normalize` fills `cacheScope: "public"`. Anything filtered per Caller must set `"private"`. `protocol.go`, `Cacheable.normalize`.
- **Bare requests work.** Stateless mode accepts a `tools/call` with no `initialize` first, and answers as SSE (`event: message` / `data: {...}`) when the client accepts both JSON and event streams.
- **Tool errors.** An error returned from a typed tool handler becomes a result with `IsError: true` and the error text as content, so error messages reach the Caller. `server.go`, `toolForErr`.

## tailcfg

- **`UnmarshalCapJSON` isn't strict.** It uses plain `json.Unmarshal`, so unknown fields are ignored. `grantValue.UnmarshalJSON` in `internal/authz` adds `DisallowUnknownFields`. `tailcfg/tailcfg.go`.
- **One flat list.** Values from every matching Grant arrive as one `[]RawMessage` under the capability name.

## tsnet

- **Peer API on every host interface.** With no tunnel interface, the peer API server binds `*:<port>` on the host. Build with `-tags ts_omit_peerapiserver` (ADR 0003). `ipn/ipnlocal/peerapi.go`, `peerAPIServer.listen`.
- **`Listen` is in-memory.** `tsnet.Server.Listen("tcp", ":80")` listens inside tsnet's netstack, not on the host.
- **Logs.** `Logf` is the verbose backend log, discarded when nil. `UserLogf` carries user-facing lines (the login URL, state changes) and defaults to `log.Printf`.
- **State.** Node state is `<Dir>/tailscaled.state`. Without an auth key, tsnet logs `To start this tsnet server, restart with TS_AUTHKEY set, or go to: <login URL>`.
- **Tailnet name.** `Up` returns `ipnstate.Status`, whose `CurrentTailnet.Name` names the tailnet the node joined.

## Shopify Admin API (`2026-10`)

- **Unused cost is refunded at once.** Shopify throttles a query on its requested cost, then charges only its actual cost: a query that requested 827 points was charged 6, and the `currentlyAvailable` it reported already counted the refund. So the client waits for a query's maximum cost, but queries drain the bucket only by what they really cost.
- **The dev store's bucket.** The dev store is on the Plus plan: 20000 points, refilling at 1000 a second. Other plans differ; the client learns the figures from each response.
- **Ledger document URIs on a move.** `inventoryMoveQuantities` requires a `ledgerDocumentUri` on a non-available side (`INVALID_QUANTITY_DOCUMENT`) and refuses one on `available` (`INVALID_AVAILABLE_DOCUMENT`). An explicit null on the `available` side is accepted, in either direction, so the move Tool's one fixed document takes a nullable ledger URI per side.
- **Moves can go negative.** A move of more than the source state holds, with both `changeFromQuantity` values right, is applied: 4 damaged units moved when 3 were there left damaged at -1. Only storewarden's `quantity ≤ expected_from_quantity` check stops it.
- **A failed key isn't "previous attempt failed".** Resending a move that failed with a user error, same key and input, got the same user error again, not `IDEMPOTENCY_PREVIOUS_ATTEMPT_FAILED`.
- **Draining it on purpose.** An object field costs 1 whether or not the Store holds much data, so 990 aliases of `shop { id }` cost the full 990. `scripts/record-throttled.sh` uses this.
