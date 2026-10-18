# Build without tsnet's peer API server

storewarden is built with `-tags ts_omit_peerapiserver`, and refuses to start without it. Every Tailscale node runs a peer API for features such as sending files and exit-node DNS. Under tsnet, which has no tunnel interface, that server's listener binds to every host interface, not just the tailnet. This was seen on macOS with tailscale.com v1.104.1. It closes connections that don't come from tailnet peers, but it is still a socket on the LAN, and it breaks the rule that storewarden opens no host listener. storewarden uses none of the peer API's features, so leaving the server out costs nothing.

## Considered Options

- **Accept the listener because it checks WhoIs.** Rejected. "Never public" should be a property of what's listening, not of a check inside Tailscale's code that storewarden doesn't control.
- **Rely on the build tag alone.** Rejected. A plain `go build` or `go run` would quietly bring the listener back, so `main` checks `buildfeatures.HasPeerAPIServer` and stops.

## Consequences

Every build, including CI and the smoke test, passes `-tags ts_omit_peerapiserver`. Revisit this ADR if tsnet stops binding the peer API to host interfaces, or if storewarden ever needs a peer API feature.
