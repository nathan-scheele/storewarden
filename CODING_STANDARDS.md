# Coding standards

The reviewer's rules. Each is a judgement call; fixed patterns are checked by `make check` instead.

## Plain Go

Write Go that a newcomer to the language can explain line by line, and choose the plain form over the clever one. Where an idiom isn't obvious from the code itself, such as a type alias that stops `UnmarshalJSON` calling itself or a func type that satisfies an interface, give it a one-line comment saying why it's there.

## Secrets stay out

Errors, logs and Audit records say what happened. They carry no credentials, no access tokens and no Shopify response bodies, which can hold customer data. Check wrapped errors too: look at what a `%w` brings along, such as a URL with a query string or a decoded body.

## Never public

storewarden is reachable only over the tailnet (`docs/design.md`, Listening; ADR 0003). Review every new socket, listener, tsnet option or dependency feature against that, including what the dependency does underneath (`docs/agents/dependency-notes.md`). Whatever depends on the Caller stays private to that Caller: per-Caller results are marked `cacheScope: "private"`, and no per-Caller state carries over between requests (ADR 0001).

## Reachable by a Caller

A behaviour a Caller should see (an error, a limit, a retry hint) has at least one handler-seam test that shows it reaching the Caller through the assembled server. A test at a lower seam, such as the Shopify client with an injected clock or deadline, proves the mechanism works; only the handler seam proves the server ever lets it happen.
