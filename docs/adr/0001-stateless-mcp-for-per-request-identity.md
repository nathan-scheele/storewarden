# Stateless MCP so every request is identified and authorized

The MCP endpoint runs the official Go SDK's Streamable HTTP handler with `Stateless: true`. In stateful mode, the SDK hands each tool handler the context of the HTTP request that *created* the session. A different tailnet peer that later sent the same session ID would be served as the original Caller, with no WhoIs check of its own. In stateless mode, every POST gets a fresh context from its own request, so WhoIs and authorization run on every call.

## Considered Options

- **Stateful, with our own session-to-peer pinning check.** Rejected. It means writing a security-critical check that the SDK gives no help with, and it negotiates an older MCP spec revision.
- **mark3labs/mcp-go.** Rejected. Its built-in tool filter is convenient, but it brings heavier dependencies and isn't the official SDK.

## Consequences

The server can't make requests back to the client: no sampling, no elicitation, and no GET event stream. v1's tools need none of these. Revisit this ADR if a future Tool needs them.
