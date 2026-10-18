# Dependency behaviour storewarden relies on

Facts about the MCP SDK (v1.8.0) and tsnet (tailscale.com v1.104.1) that shape storewarden's code and can't be seen from it. Each was checked against the module source in `~/go/pkg/mod`; re-check when a version changes.

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
